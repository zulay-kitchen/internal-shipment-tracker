# AGENTS.md

Context for any agent (or human) picking up work on this project. This file
describes what exists and how it's built. See `SKILLS.md` for step-by-step
patterns to follow when extending it.

## What this is

`internal-shipment-tracker` (module name - see `go.mod`; stdlib-only
dependencies) is a small Go module with two CLI commands under `cmd/`, both
built on Goflow order data:

- **`tracker`** (`cmd/tracker`):
  1. Shows a welcome menu: "Check for carrier scan" or "Help me" (prints
     usage, then asks again).
  2. Asks which carrier(s) to check - "All" or any subset of whichever
     carriers have credentials configured.
  3. Prompts for a date range.
  4. Pulls every Goflow order marked "shipped" in that range.
  5. For each shipment's tracking number(s) whose carrier was selected in
     step 2, asks that carrier's API whether the package has actually been
     physically scanned yet (not just labeled/manifested).
  6. Writes a CSV (`order id, date shipped, carrier, shipping method,
     tracking number, carrier scanned`) and a plain-text log file mirroring
     the console output.
  7. Prints a timing breakdown at the end.
- **`package-level-detail`** (`cmd/package-level-detail`): prompts for (or
  takes as an argument) a date range and writes a CSV with one row per
  package/box shipped in that range - tracking number, order number,
  carrier, shipping method, ship date, weight, dimensions, destination
  ZIP/state, and shipping charge. No carrier API calls, no log file - just
  a Goflow pull and a CSV write.

Both commands are `cmd/` binaries sharing one `internal/` package: the
Goflow API client (`internal/goflow`) is factored out so any command
binary in this module can import it - see "File layout" below - while
everything CLI-specific to each command (config/env loading, its own date
range handling, output format) stays in that command's own `main.go`.

## Build & run

- Requires Go 1.21+. No external dependencies (stdlib only) - keep it that
  way unless there's a strong reason not to, since this has been developed
  in a sandbox with no network access to fetch modules or even install a Go
  toolchain to compile-check changes (see SKILLS.md's verification section).
- `go run ./cmd/tracker` or `go build -o tracker ./cmd/tracker && ./tracker`
  (run from the repo root; `cd cmd/tracker && go run .` also works).
- Flags: `-dir <path>` (output directory for the CSV/log, default `.`,
  created automatically), `-h` / `-help` / a bare `help` argument (usage).
- Module name: `internal-shipment-tracker` (see `go.mod`); import the
  Goflow client from another command in this module as
  `internal-shipment-tracker/internal/goflow`.
- `go build ./...` / `go vet ./...` build every command in this module,
  including `cmd/package-level-detail` (see below) - no scoping needed.

## Configuration

Required: `GOFLOW_SUBDOMAIN`, `GOFLOW_API_TOKEN`.

Optional, per carrier (only added to the runnable set when ALL of that
carrier's vars are present):

- `UPS_CLIENT_ID` / `UPS_CLIENT_SECRET`
- `FEDEX_CLIENT_ID` / `FEDEX_CLIENT_SECRET`
- `USPS_CLIENT_ID` / `USPS_CLIENT_SECRET`
- `AMAZON_SHIPPING_CLIENT_ID` / `AMAZON_SHIPPING_CLIENT_SECRET` /
  `AMAZON_SHIPPING_REFRESH_TOKEN` (+ optional `AMAZON_SHIPPING_ENDPOINT`)

Any of the above can go in a `.env` file (`KEY=VALUE` per line) next to the
binary/source or in the current directory instead of real env vars; real
env vars always win. See `.env.example` for the canonical, filled-in-blank
list.

## File layout

- `cmd/tracker/main.go` - the CLI program: `env.Load()` + its own `config`/
  `loadConfig`, the interactive welcome/carrier-selection menu, the date
  prompt, carrier tracking lookups (`scanChecker` + one implementation per
  carrier), the per-carrier rate limiter, progress bars, the log file, and
  `main()` itself.
- `internal/goflow/goflow.go` - the Goflow API client (`Client`, `Order`/
  `Shipment`/`Box`/`Weight`/`Dimensions`/`Cost`/`ShippingAddress`,
  `FetchShippedOrders`), factored out so any command in this module can
  import it as `internal-shipment-tracker/internal/goflow` without pulling
  in `cmd/tracker`'s CLI-specific code. It's deliberately decoupled from the
  CLI's console/log-file plumbing: instead of taking a `*os.File` to log to,
  `Client.Notice` is a plain `io.Writer` that callers point at whatever
  fan-out they want (`cmd/tracker` points it at `dualWriter(os.Stdout,
  logFile)`; `cmd/package-level-detail` points it straight at `os.Stdout`; a
  caller with no logging needs can just leave it `nil`). `Notice` gets both
  rate-limit-retry lines and a line per page pulled (see below) - one
  writer for everything the client itself wants to report, since every
  caller so far wants both fanned into the same place anyway.
- `internal/env/env.go` - the `.env` file loader (`Load`), factored out of
  `cmd/tracker` the same way `internal/goflow` was: both commands need
  identical `.env`-search-then-apply behavior, and unlike the Goflow client
  this one has no CLI-specific parts to keep separate, so it's just a single
  `Load()` call with nothing to configure.
- `cmd/package-level-detail/main.go` - the package-level-detail CLI:
  `env.Load()` + its own minimal `config` (just the two Goflow vars - it
  doesn't need carrier credentials at all), its own date-range parsing
  (`parseDateRange`/`promptDateRange`, accepting the range as a CLI argument
  or falling back to an interactive prompt), a `goflow.Client` pull, and a
  CSV write (`boxRow` builds one row per box - `csvHeader` fixes the column
  order). Note the directory was originally created as `packag-level-detail`
  (missing an "e") and has since been renamed to match this section's
  heading.
- `go.mod` - module `internal-shipment-tracker`, Go 1.21, no dependencies.
- `.env.example` - copy to `.env` and fill in.
- `README.md` - user-facing docs (setup, flags, run instructions, notes and
  caveats). Keep this in sync with behavior changes.
- `AGENTS.md` / `SKILLS.md` - this file and its companion.

## Architecture

### `internal/env/env.go`

- `Load()` is the only exported function: searches the compiled binary's
  directory, then the current working directory, for a `.env` file, and
  applies each `KEY=VALUE` line via `os.Setenv` - but only for keys not
  already set in the real environment, so real env vars always win.
  Missing file is not an error. `searchDirs`/`applyFile`/`unquoteValue` are
  private helpers behind it.
- No config/credential knowledge lives here at all - it just mutates
  `os.Environ()`; each command still has its own `config`/`loadConfig` that
  calls `os.Getenv` afterward for the specific variables it cares about.

### `internal/goflow/goflow.go`

- `Order`/`Shipment`/`Box` - the subset of Goflow's order/shipment JSON shape
  this program needs.
- `buildRawQuery` (keeps `filters[...]` keys unencoded) and `retryAfterDelay`
  (parses `Retry-After`) are private helpers.
- `Client` wraps a subdomain/token/`*http.Client` plus an optional
  `Notice io.Writer` and a private `notef` helper (no-ops if `Notice` is
  `nil`, otherwise `fmt.Fprintf`s to it); `doRequest` retries forever on 429
  using `Retry-After`, calling `notef` once per retry; `FetchShippedOrders`
  paginates through `status=shipped` + `status_updated_at`-range orders -
  Goflow has no `shipment.shipped_at` filter, so callers (`cmd/tracker`'s
  `main`) re-check the real `shipped_at` client-side - and calls `notef`
  once per page ("Goflow contacted and N orders pulled.") plus once more
  right after if another page follows ("More Goflow orders indicated."),
  so a caller watching `Notice` can tell pagination is still in progress
  rather than the pull having stalled.

### `cmd/tracker/main.go` (top to bottom)

Each section is marked with a `// ---...---` banner comment in the file
itself; this is the same order:

1. **Configuration** - `config` struct, `loadConfig` (env vars are already
   in the process environment by the time this runs - see `main()` below).
2. **Interactive menu** - `menuCheckCarrierScan`/`menuHelpMe` (named option
   numbers for the welcome menu), `promptMenu` (generic numbered-list
   prompt, re-prompts on a bad answer instead of failing), and
   `promptCarrierSelection` (the "All" + per-carrier multi-select prompt,
   built on the same numbered-list convention, parsing comma-separated
   numbers). Both take a `*bufio.Reader` rather than an `io.Reader` -
   see the note on `promptMenu` and the "shared stdin reader" bullet under
   `main()` below for why that's not just `promptDateRange`'s own
   `bufio.NewReader(in)` pattern repeated.
3. **Date range prompt** - `promptDateRange`, `dateRangeRe`. Accepts either
   `YYYY-MM-DD to YYYY-MM-DD` or a single `YYYY-MM-DD` (end defaults to
   today, UTC). Also takes the shared `*bufio.Reader`, for the same reason.
4. **Carrier tracking lookups** - `retryAfterDelay` (a private copy of
   `internal/goflow`'s helper - USPS's checker needs the same `Retry-After`
   parsing and there was no reason to make `cmd/tracker` import
   `internal/goflow` just for a 10-line utility), then the `scanChecker`
   interface
   (`Ping(ctx) error`, `Scanned(ctx, trackingNumber) (bool, error)`),
   `detectCarrierFromTrackingNumber` (regex-based UPS/FedEx/USPS format
   detection used to bypass Amazon SP-API when possible), then
   `rateLimiter`/`carrierRateLimit`/`carrierRateLimits`/`rateLimitFor`
   (per-carrier token-bucket rate limiting, see below), then one
   struct+constructor+`Ping`+`Scanned` per carrier: `upsChecker`,
   `fedexChecker`, `uspsChecker`, `amazonShippingChecker`. Every `Ping` is
   the same one-liner - call `token(ctx)`, discard the token, return the
   error - since credential validity for all four carriers just means "can
   this client ID/secret (or refresh token) get an OAuth token."
5. **Console progress bars** - `multiProgress` (ANSI in-place line updates),
   `renderProgressBar`, `renderProgressText`.
6. **Log file** - `dualWriter` (console + log file fan-out),
   `formatDuration` (ms/sec/min formatting for the timing breakdown).
7. **main** - `printUsage`, then `main()` itself:
   - Parse flags, handle `-h`/`-help`/`help`, create `-dir`.
   - Call `env.Load()`, then `loadConfig()`; build the `checkers` map (only
     for carriers with full credentials), fix `knownCarriers` order (`ups`,
     `fedex`, `usps`, `amazon_shipping`).
   - Create one `stdin := bufio.NewReader(os.Stdin)` and pass it to every
     interactive prompt for the rest of the run (welcome menu, carrier
     selection, date range) - each prompt creating its own reader from
     `os.Stdin` would risk silently losing whatever a previous prompt's
     read call had already buffered past the line it consumed.
   - **Welcome menu**: loop `promptMenu` ("Check for carrier scan" /
     "Help me") until the user picks the former; "Help me" prints usage via
     `printUsage(os.Stdout)` and loops back to the menu instead of exiting.
   - **Carrier selection**: build `availableCarriers` from `knownCarriers`
     filtered to what's actually in `checkers` (i.e. has credentials
     configured), error out if that's empty, then call
     `promptCarrierSelection`. Any carrier not in the result gets `delete`d
     from `checkers` - from here on a deselected carrier is indistinguishable
     from one with no credentials at all, the same trick the ping pass
     below relies on.
   - Prompt for the date range, open the log file (named
     `logs_shipped_orders_<start>_<end>.txt`).
   - **Ping pass**: for each `knownCarriers` entry present in `checkers`,
     call its `Ping(ctx)` and log `<carrier> ping ok` or `ping FAILED - ...`
     via `dualWriter`. A failed ping `delete`s that carrier from `checkers`
     right there - every later step (progress display, pass 1's `supported`
     check, the per-carrier tally, the high-error-rate `ALERT`) only ever
     looks at `checkers`, so this one `delete` is enough to make a
     bad-credentials carrier behave identically to a never-configured one
     for the rest of the run. This is sequential (at most 4 carriers, one
     HTTP call each) - no worker pool needed here unlike the tracking
     lookups below.
   - Build a `goflow.NewClient(cfg.goflowSubdomain, cfg.goflowToken)`, point
     its `Notice` at `dualWriter(os.Stdout, logFile)`, then call
     `goflowClient.FetchShippedOrders(ctx, start, endExclusive)`, timed.
   - **Pass 1** (single-threaded, no network calls): for every order and
     tracking number, resolve the "lookup carrier" (handles the
     amazon_shipping fallback), build de-duplicated per-carrier queues, and
     build `pending` rows.
   - Build the initial progress display (one line per `knownCarriers`
     entry: live bar, or "missing credentials"/"no tracking numbers" with a
     real `0/<total>` count).
   - Spawn one goroutine per carrier that has credentials and a non-empty
     queue - carriers run concurrently with each other. Within a carrier's
     goroutine, tracking numbers are fanned out over a channel to a small
     worker pool (`rateLimitFor(carrier).concurrency` workers), and every
     worker calls `limiter.Wait(ctx)` before each lookup so the carrier's
     sustained request rate stays under `rateLimitFor(carrier).rps` no
     matter how many workers are running. Timed per carrier (the timer
     spans the whole worker pool, not a single lookup).
   - `wg.Wait()`, then print/log errors and a per-carrier tally (only now,
     so nothing corrupts the live ANSI bars while they're active). If a
     carrier's error rate is at or above `highErrorRateThreshold` (90%)
     over at least `minLookupsForHighErrorRateAlert` (5) lookups, an
     `*** ALERT: ... ***` block is also printed for it - a rate limit or a
     few bad tracking numbers fails a small fraction of calls, not nearly
     all of them, so near-total failure means something systemic (bad
     credentials, no API access authorization, ...) rather than an ordinary
     hiccup.
   - **Pass 2**: turn `pending` + lookup results into final CSV rows.
   - Write the CSV (`shipped_orders_<start>_to_<end>.csv`), timed.
   - Print the timing breakdown (fetch, per-carrier, write, total) to both
     console and the log file.

## Design decisions worth knowing before changing anything

- **No external dependencies, on purpose.** This was built without network
  access to fetch Go modules or even install a Go toolchain in the dev
  sandbox, so everything is stdlib. Don't add a dependency without a strong
  reason and calling it out. (It started as a literal single file; it's now
  `cmd/tracker/main.go` + `internal/goflow/goflow.go` - see "File layout" -
  but the "no dependencies" part of the original reasoning still applies.)
- **Carrier "scanned" logic is best-effort and explicitly flagged as such**
  in a comment right above each `return` in each carrier's `Scanned`
  method - naming the exact status code/field relied on and asking future
  maintainers to re-verify against that carrier's current docs if results
  look wrong. Carrier APIs change; this codebase assumes they will.
- **A carrier without credentials, or a Goflow carrier value with no
  implementation at all, never fails the run** - it just leaves "carrier
  scanned" blank for those rows and logs a one-time note.
- **Console and log file agree**, except for the live ANSI progress bars
  themselves (those would be unreadable noise in a text file - a clean
  before/after snapshot is logged instead). Use `dualWriter(...)` for
  anything that should hit both.
- **Progress-bar alignment matters to the user** - active bars and inactive
  ("missing credentials" / "no tracking numbers") lines must render in
  identical columns; that's why `renderProgressText` mirrors
  `renderProgressBar`'s layout instead of using a different format.
- **No compiler was available while building the bulk of this** (sandboxed,
  no network). Early changes were verified by hand: counting `(`/`)` and
  `{`/`}` balance, re-reading changed regions fully, and grepping for stale
  references after renames - not by actually running `go build`. A Go
  toolchain has since become available in this environment (`go build ./...`,
  `go vet ./...`, `gofmt -l .` all work cleanly) - always use it for new
  changes instead of hand-verification; don't assume older code has actually
  been compiler-verified just because it now compiles unchanged.
- **Per-carrier concurrency is bounded by a token-bucket rate limiter**
  (`rateLimiter`, `carrierRateLimits`, `rateLimitFor` - just above the UPS
  section). Each carrier's queue is worked by a small pool of goroutines
  (`carrierRateLimit.concurrency`) instead of a single one, but every worker
  calls `limiter.Wait(ctx)` before its API call, capping the *sustained*
  rate at `carrierRateLimit.rps` regardless of pool size. The numbers in
  `carrierRateLimits` are conservative guesses, not confirmed contracted
  limits - re-tune them (see SKILLS.md) if a carrier is either getting
  429'd or clearly has more headroom.

## Known limitations (see README's "Notes / caveats" for the full list)

- Only `ups`, `fedex`, `usps`, and `amazon_shipping` have real tracking
  lookups. Every other Goflow carrier value is left blank.
- The Amazon Shipping checker calls SP-API with just an LWA bearer token,
  no AWS SigV4 signing - may need signing added depending on the seller
  application's authorization type.
- Rate-limit retry (`Retry-After`, 429) is implemented for Goflow
  (`internal/goflow`'s `Client.doRequest`) and USPS specifically (both
  documented it); UPS/FedEx/Amazon Shipping don't retry on 429 - they rely
  solely on the per-carrier rate limiter in `carrierRateLimits` to avoid
  triggering one in the first place.

## History

This project started life as `goflow-shipped-report` (module
`goflowshippedreport`) and was renamed to `tracker` to be easier to type.
Because generated files in this environment can't be deleted or renamed
once written, the old `goflow-shipped-report/` folder still exists
alongside this one - it's stale and unmaintained. All current work lives in
this folder.

The module was later renamed again, to `internal-shipment-tracker` (see
`go.mod`), when the layout changed from a single `main.go` at the repo root
to `cmd/tracker` + `internal/goflow` + `cmd/package-level-detail` (see
"File layout"). Import paths for `internal/goflow` are
`internal-shipment-tracker/internal/goflow`, not `tracker/internal/goflow` -
the `tracker` name now refers only to the `cmd/tracker` binary, not the
module.
