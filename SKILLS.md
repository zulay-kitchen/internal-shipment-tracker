# SKILLS.md

Step-by-step patterns for extending `tracker`, so new work matches the
conventions already established rather than inventing new ones. Read
`AGENTS.md` first for the overall architecture; this file is the "how do I
add X" reference.

Unqualified references to `main()`/`config`/etc. below mean
`cmd/tracker/main.go` specifically, not `internal/goflow` or `internal/env`
- see the next two sections for those.

## How to load environment variables in a new command

- Call `env.Load()` (from `internal/env`, `internal-shipment-tracker/internal/env`)
  once near the top of `main()`, before reading any env vars - see both
  `cmd/tracker` and `cmd/package-level-detail` for the pattern. It checks
  for a `.env` file next to the binary/source or in the current directory
  and applies it to `os.Environ()`; a missing file is not an error.
- `env.Load()` only touches the process environment - it has no opinion on
  which variables your command needs. Define your own `config`
  struct/`loadConfig()` afterward (see either command's "Configuration"
  section) that calls `os.Getenv` for exactly the variables that command
  requires, and validates/defaults them there.
- Don't add command-specific variables or validation to `internal/env`
  itself - it's intentionally credential-agnostic so it stays reusable by
  any future command in this module. If a new command needs a new env var,
  that lives in that command's own `config`.

## How to add a welcome-menu option

- Add the new label to the `options` slice passed to `promptMenu` in
  `main()`, and a named constant for its 1-based position alongside
  `menuCheckCarrierScan`/`menuHelpMe` - keep the constant names and the
  slice order in sync, since nothing enforces that for you.
- Handle it inside the welcome-menu `for` loop: `menuHelpMe` is handled by
  printing usage and `continue`-ing back to the prompt; a brand new option
  that isn't just "do the one existing thing" should probably `break` out
  under its own case (or `return` early) rather than falling into the
  carrier-selection/date-range flow that currently follows the loop
  unconditionally.
- If the new option doesn't need carrier selection and/or a date range at
  all, restructure so those prompts only run for `menuCheckCarrierScan` -
  right now they're unconditional after the loop because that's the only
  path that exists.
- Reuse `promptMenu` for any new numbered-choice prompt (single answer) and
  `promptCarrierSelection`'s pattern (comma-separated numbers, an "All"-like
  catch-all as option 1) for any new multi-select prompt, rather than
  hand-rolling another parsing loop - see either's doc comment.
- Every interactive prompt in `main()` shares one `*bufio.Reader` (`stdin`,
  built once near the top of `main()`) - pass that same variable into any
  new prompt call. Do not call `bufio.NewReader(os.Stdin)` again inside a
  new prompt function: an earlier prompt's read may have buffered bytes
  past the line it consumed, and a fresh reader would silently drop them,
  eating a later prompt's answer (this actually happened during manual
  testing while adding the carrier-selection prompt - multiple sequential
  prompts is exactly the scenario that surfaces it).

## How to add functionality to the Goflow API client

- New Goflow endpoints/fields belong in `internal/goflow/goflow.go`, as a
  method on `Client` (see `FetchShippedOrders` for the pattern: build the
  URL, call `c.doRequest`, unmarshal into an exported type).
- Keep the package decoupled from `cmd/tracker`'s CLI assumptions - no
  `*os.File`, no `dualWriter`, no direct `fmt.Println`. The one exception is
  `Client.Notice io.Writer` (via the private `c.notef(format, args...)`
  helper, which no-ops if `Notice` is `nil`): route anything worth telling
  a caller about through it - rate-limit/retry lines, a line per page
  pulled, and so on - instead of inventing a second logging mechanism or a
  second field. One writer for everything the client reports keeps it a
  single knob for callers to wire up.
- New response fields go on exported structs (`Order`, `Shipment`, `Box`,
  or new ones) with the same `json:"..."` tag style already used.
- Add a manual smoke test (temporary `_test.go` file reading `.env` and
  calling the new method against the real API, deleted once it passes) if
  you can't otherwise convince yourself the request/response shapes are
  right - `go vet`/`go build` only catch compile errors, not a wrong field
  name that still unmarshals to a zero value.

## How to add a new carrier's tracking lookup

1. Add a `<carrier>Checker` struct implementing the `scanChecker` interface
   (`Ping(ctx context.Context) error`, `Scanned(ctx context.Context,
   trackingNumber string) (bool, error)`), modeled on
   `upsChecker`/`fedexChecker`/`uspsChecker`/`amazonShippingChecker`:
   - Fields for credentials + `httpClient *http.Client`.
   - A mutex-guarded `accessToken`/`expiresAt` pair with a `token(ctx)`
     method that returns the cached token if still valid, otherwise does
     the OAuth exchange and caches the result (refresh ~60s early).
   - A `new<Carrier>Checker(...)` constructor.
   - A `Ping` method that's just `_, err := c.token(ctx); return err` - see
     any existing checker's `Ping` for the one-liner. Don't call the actual
     tracking endpoint from `Ping`; it only needs to prove the credentials
     themselves work, not that a specific tracking number is trackable.
   - A `Scanned` method that calls the carrier's tracking endpoint and
     returns whether at least one *physical* scan has happened (not just a
     label/manifest).
2. Add credential fields to the `config` struct and read them via
   `os.Getenv` in `loadConfig()`. Add matching entries to `.env.example`,
   `printUsage()`'s "Configuration" list, and README's env var tables.
3. In `main()`, construct the checker into the `checkers` map only when
   *all* of its required credentials are non-empty - never partially
   (mirrors the existing `if cfg.xClientID != "" && cfg.xClientSecret !=
   "" { ... }` pattern).
4. Add the carrier's exact Goflow `shipment.carrier` enum value (e.g.
   `"dhl_ecommerce"`) to the `knownCarriers` slice in `main()`. This one
   list drives the progress-bar display, the post-run tally, and the log
   snapshot - nothing else needs to know about a new carrier.
4a. Add an entry for it to `carrierRateLimits` (near the `rateLimiter` type,
   just above the UPS section) with a conservative `rps` (sustained
   requests/second) and `concurrency` (worker pool size / burst) - check
   the carrier's API docs for a documented rate limit first, and err low if
   it doesn't publish one. Skipping this isn't a hard failure
   (`defaultCarrierRateLimit` covers any carrier missing from the map) but
   the fallback is deliberately conservative, so add a real entry instead
   of relying on it.
5. Document the scan-detection heuristic in a comment directly above the
   `return` statement that makes the call, naming the specific field/status
   code relied on and telling future maintainers to re-verify it against
   that carrier's current API docs if results ever look wrong. Match the
   tone/style of the existing UPS/FedEx/USPS/Amazon comments.
6. Update README: the "Only UPS, FedEx, USPS, and Amazon Shipping..."
   bullet and the "has it been scanned" status-codes bullet in Notes /
   caveats, plus the Setup section's env var block.

## How to add a format-detection fallback (like amazon_shipping -> UPS/FedEx/USPS)

- Add a conservative regex to the `var (...)` block near
  `detectCarrierFromTrackingNumber` - conservative means it must not
  overlap with any other carrier's pattern.
- Wire it into pass 1 of `main()`, inside the `carrier == "amazon_shipping"`
  branch (or a new sibling branch for another carrier) - only redirect to a
  carrier that's actually configured (`checkers[detected]` exists), and
  print a one-time note the first time it fires per detected carrier (see
  the `fallbackNoted` map).

## How to add a new CLI flag

- Register it with the standard `flag` package at the top of `main()`,
  before `flag.Parse()` (see the `-dir` flag for the pattern).
- `flag.PrintDefaults()` inside `printUsage()` picks it up automatically -
  just double-check the surrounding help text still reads sensibly.
- Update README's "Flags" section, and the "Interactive prompt" / file
  output sections if the new flag affects where/what gets written.

## How to add a new environment variable

- Add a field to `config`, read it with `os.Getenv` in `loadConfig()`, and
  validate/default it there if needed (see `amazonShippingEndpoint`'s
  default-value pattern).
- Add it to `.env.example` with a comment noting optional/required, to
  `printUsage()`'s "Configuration" block, and to README's setup section.

## How to control what goes into the log file

- `dualWriter(os.Stdout, logFile)` / `dualWriter(os.Stderr, logFile)` -
  wrap any `fmt.Fprint*` destination in this when the message should appear
  on console *and* in the log file. This is the only mechanism that should
  be used for "user-facing" messages after the log file is created.
- `fmt.Fprintln(logFile, ...)` / `fmt.Fprintf(logFile, ...)` directly
  (no `dualWriter`) for log-only content - this is how the ANSI progress
  bars are kept out of the log file: a clean plain-text snapshot is written
  before the concurrent lookups start and a tally is written after they
  finish, instead of mirroring the live bars themselves.
- `logFile` (`*os.File`) doesn't exist until the date range is known (its
  filename embeds `<start>_<end>`) - nothing before that point in `main()`
  can log to file, only to the console directly.

## How to add another phase to the timing breakdown

- Capture `start := time.Now()` immediately before the phase and
  `duration := time.Since(start)` immediately after. Three patterns already
  exist to copy from:
  - Linear step, single duration: `fetchDuration` (fetch), `writeDuration`
    (CSV write).
  - One-per-goroutine, written to a shared slice by index (no locking
    needed since each goroutine only touches its own index):
    `carrierDurations`, indexed via `lineIndex[carrier]`.
- Add a line to the "Timing breakdown" block near the end of `main()`:
  `fmt.Fprintf(out, "  %-24s %s\n", "<Label>", formatDuration(duration))`.
- Always go through `formatDuration` - never hardcode "ms"/"sec"/"min"
  formatting inline, so the thresholds only ever need to change in one
  place. Current thresholds: milliseconds under 1000ms, seconds (one
  decimal) up to 120 sec, minutes (one decimal) beyond that.

## How to add or adjust a progress-bar line

- Every carrier line is rendered by exactly one of:
  - `renderProgressBar(carrier, done, total)` - for an actively-running
    lookup.
  - `renderProgressText(carrier, text, done, total)` - for anything
    inactive (missing credentials, nothing queued, etc.). It pads/truncates
    `text` to `progressBarWidth` (24 chars) so the columns match
    `renderProgressBar` exactly, and still shows a real `done/total` count.
- Never hand-roll a one-off `fmt.Sprintf` for a new inactive state - use
  `renderProgressText` so alignment is guaranteed to match.
- Keep new status strings short enough to fit in `progressBarWidth`
  characters without an awkward mid-word truncation (e.g. "no tracking
  numbers", not "no tracking numbers to check").
- Line order/position is fixed by `knownCarriers` and tracked in
  `lineIndex`. Never print anything else to stdout between
  `newMultiProgress(...)` and `wg.Wait()` - it will corrupt the in-place
  ANSI redraws. Anything that must be logged during that window should go
  to `logFile` directly (see the "log file" section above), not to stdout.

## Verification checklist after any change

A Go toolchain is available in this environment - always use it:

```
gofmt -l .
go vet ./...
go build ./...
```

(For a quick manual run of just one binary: `go build -o /tmp/tracker_build
./cmd/tracker` or `go build -o /tmp/pld_build ./cmd/package-level-detail`.)

Treat the three commands above as the real verification step, not a
substitute for reading the diff. If you ever end up somewhere without a
toolchain (no network access to install one), fall back to
hand-verification instead:

1. Counting `(`/`)` and `{`/`}` balance across non-comment lines in
   whichever file you changed, e.g. for `cmd/tracker/main.go`:
   ```
   python3 -c "
   lines = open('cmd/tracker/main.go').read().split('\n')
   code = '\n'.join(l for l in lines if not l.strip().startswith('//'))
   print('parens', code.count('('), code.count(')'))
   print('braces', code.count('{'), code.count('}'))
   "
   ```
   This catches gross mismatches, not real compile errors.
2. Reading the entire changed region back afterward and manually tracing
   types, scoping, and variable shadowing.
3. Grepping for the old symbol/string after any rename to confirm no stale
   references remain anywhere in the file or docs.

Some of this codebase's earlier history was written that way (see
AGENTS.md's design decisions) and has not necessarily been confirmed to
compile since - don't assume unchanged old code is compiler-verified just
because a change elsewhere built successfully.

## Naming / history note

The project was originally scaffolded as `goflow-shipped-report` (module
`goflowshippedreport`) and later renamed to `tracker` (module `tracker`,
this folder) for a shorter command-line name. Generated files in this
environment can't be deleted or renamed once written, so the old
`goflow-shipped-report/` folder still exists on disk - it's stale and
should not be edited. All current and future work happens in this folder.

The module was renamed again since, to `internal-shipment-tracker`, when a
second command (`cmd/package-level-detail`) was added alongside `cmd/tracker`
and the Goflow client moved out to `internal/goflow`. Import it as
`internal-shipment-tracker/internal/goflow` - `tracker` is now just the name
of one binary (`cmd/tracker`), not the module.
