# tracker

Opens with a menu to pick which carrier(s) to check and a date range, pulls
shipped Goflow orders in that range, and writes a CSV (plus an Excel copy of
the same data): `order id, date shipped, carrier, shipping method, tracking
number, carrier scanned`.

One row per tracking number (an order that ships in 3 boxes gets 3 rows).
`carrier scanned` is `yes` if the carrier's API shows a physical scan
(package actually received into their network, not just a label/manifest
sitting there), otherwise blank.

## Setup

Requires Go 1.21+.

Copy `.env.example` to `.env` in this same directory and fill it in, or set
these as real environment variables (real env vars always win over the
`.env` file):

```
GOFLOW_SUBDOMAIN=companyname       # from companyname.goflow.com
GOFLOW_API_TOKEN=...               # Goflow Settings -> API Tokens
```

Optional, one pair per carrier you want checked (any carrier without
credentials is left blank in the "carrier scanned" column instead of erroring):

```
UPS_CLIENT_ID=...
UPS_CLIENT_SECRET=...

FEDEX_CLIENT_ID=...
FEDEX_CLIENT_SECRET=...

USPS_CLIENT_ID=...
USPS_CLIENT_SECRET=...

AMAZON_SHIPPING_CLIENT_ID=...
AMAZON_SHIPPING_CLIENT_SECRET=...
AMAZON_SHIPPING_REFRESH_TOKEN=...
AMAZON_SHIPPING_ENDPOINT=...   # optional, defaults to the NA SP-API endpoint
```

These are each carrier's own developer-portal OAuth client credentials, not
your Goflow token. UPS/FedEx/USPS use a client_credentials grant (just
client ID + secret). Amazon Shipping is SP-API, which requires a
refresh_token grant tied to a seller's authorization, so all three of
AMAZON_SHIPPING_CLIENT_ID/_SECRET/_REFRESH_TOKEN must be set together or
Amazon Shipping lookups are skipped.

Before fetching orders, the program pings every carrier that has credentials
configured - just an OAuth token request, logged as `<carrier> ping ok` or
`ping FAILED` - and drops any carrier whose ping fails for the rest of that
run, the same as if it had no credentials at all, instead of spending time
on tracking lookups that would only fail the same way.

Once orders are fetched, the program shows one line per carrier: a live
progress bar while that carrier's tracking numbers are being checked, or
`missing credentials` if it doesn't have what it needs to run at all (this
now covers both "no credentials configured" and "credentials configured but
failed the ping"). All configured carriers are checked in parallel, and
within a single carrier several lookups run concurrently too - each carrier
has its own worker pool size and requests-per-second cap (see "Notes /
caveats" below), so it gets real concurrency without exceeding that
carrier's rate limit.

## Run

From the repo root:

```
go run ./cmd/tracker
```

or build a binary and run that (`go build -o tracker ./cmd/tracker && ./tracker`).
`cd cmd/tracker && go run .` works too, if you'd rather run it from there.

### Flags

```
-dir string   directory to write the CSV and log file to (default ".",
              created automatically if it doesn't already exist)
-h, -help     show usage and exit
```

`help` as the only argument works the same as `-h` (e.g. `go run ./cmd/tracker help`).

### Interactive prompts

It opens with a welcome menu:

```
Welcome! What would you like to do?
  1. Check for carrier scan
  2. Help me
Enter a number:
```

`2` (`Help me`) prints the same usage text as `-h`/`-help`/`help`, then asks
the welcome menu question again - it's a way to see that text without
already knowing about the `-h` flag. `1` (`Check for carrier scan`, the only
thing this program actually does) moves on to carrier selection:

```
Which carriers would you like to check?
  1. All
  2. ups
  3. usps
Enter one or more numbers separated by commas (e.g. "2,3"):
```

Only `All` plus whichever carriers actually have credentials configured (see
Setup above) are listed - it always includes `All`, then each configured
carrier in a fixed order (`ups`, `fedex`, `usps`, `amazon_shipping`). Answer
with one number for a single carrier, several comma-separated numbers for a
few, or `1` for all of them. A carrier you don't select is treated exactly
like a carrier with no credentials configured at all for the rest of that
run: it's skipped entirely (not even pinged - see below), and its "carrier
scanned" column stays blank.

Finally, it asks for the date range:

```
Enter date range (YYYY-MM-DD to YYYY-MM-DD, or just YYYY-MM-DD to use today as the end date):
```

A single date (e.g. `2026-01-01`) is shorthand for "from that date through
today" - the end date defaults to today (UTC) when omitted.

Any invalid answer at any of these three prompts just asks again instead of
exiting, so a typo doesn't require restarting the whole program.

It will then write three files into `-dir` (the current directory by
default): `tracking_report_<start>_to_<end>.csv`,
`tracking_report_<start>_to_<end>.xlsx`, and
`logs_shipped_orders_<start>_<end>.txt`. Both the CSV and the Excel file
have identical contents (the `.xlsx` is converted straight from the CSV)
and are sorted by carrier, then by ship date within each carrier. If the
`.xlsx` conversion fails for some reason, the CSV is unaffected - a warning
is printed/logged instead of the run failing. The log file is a plain-text
mirror of everything printed to the console (minus the live progress bars'
ANSI redraws, which are replaced with a clean before/after summary per
carrier) - if something goes wrong, send that file along and it'll show
what happened, including any lookup errors.

Finally, it prints a timing breakdown (also mirrored into the log file):
how long fetching orders from Goflow took, how long each carrier's tracking
lookups took, how long writing the CSV/log took, and the total run time.
Each is shown in milliseconds under 1000ms, seconds (one decimal place) up
to 120 sec, and minutes (one decimal place) beyond that (e.g. `420 ms`,
`2.3 sec`, `3.5 min`).

## Notes / caveats

- The `.xlsx` file is written by this repo's own `internal/xlsx` package -
  stdlib only, no third-party Excel library - by converting the CSV after
  it's written. It's a single plain sheet: every cell is text (no real
  number/date types, no formatting, no column widths), so numeric columns
  will be left-aligned in Excel and won't be directly summable without
  first converting them - open the CSV instead if you need real numbers to
  do math on.
- Goflow's API doesn't support filtering directly on `shipment.shipped_at`,
  so orders are first fetched by `status=shipped` + `status_updated_at` in
  range, then filtered client-side against the real `shipment.shipped_at`.
  This should be accurate for the vast majority of orders but flag it if
  numbers look off.
- Every Goflow order pull logs `Goflow contacted and N orders pulled.`; if
  the results were paginated, a `More Goflow orders indicated.` line
  appears between pages, right before the next page is requested - so a
  pull that's taking a while shows visible progress instead of going quiet
  until it's entirely done.
- Only UPS, FedEx, USPS, and Amazon Shipping (`amazon_shipping`) tracking
  lookups are implemented, each in its own package under
  `internal/carriers` (`ups`, `fedex`, `usps`, `amazon`). Other carriers in
  Goflow's carrier list (DHL, Canada Post, Purolator, Amazon Logistics,
  etc.) will always show a blank "carrier scanned" column. The
  `scanChecker` interface in `cmd/tracker/main.go` is there to make adding
  another carrier package straightforward.
- `amazon_shipping` is often just a relabeled UPS/USPS/FedEx shipment
  (Amazon's Buy Shipping service resells those carriers' rates). Before
  falling back to Amazon SP-API, `detectCarrierFromTrackingNumber` in
  `cmd/tracker/main.go` checks whether the tracking number's own format
  unambiguously matches UPS/FedEx/USPS (via each carrier package's own
  `LooksLikeTrackingNumber`) and, if so, looks it up through that carrier's
  API instead - no Amazon credentials needed for those. This only kicks in
  for tracking numbers with a distinctive-enough shape; anything else still
  needs `AMAZON_SHIPPING_*` credentials.
- The "has it been scanned" logic per carrier is a best-effort read of each
  carrier's tracking status codes (UPS activity `status.type`, FedEx
  `scanEvents` codes, USPS `statusCategory`, Amazon Shipping `eventHistory`
  / `summary.status`), implemented in that carrier's own
  `internal/carriers/<carrier>/<carrier>.go`. Carrier APIs change their
  schemas occasionally - if results look wrong, check the relevant comment
  above the `return` in that carrier's `Scanned` method against that
  carrier's current API docs. Amazon Shipping in particular is implemented
  without AWS SigV4 request signing (relying on the LWA access token
  alone); if Amazon's SP-API rejects that for your application type,
  signing will need to be added.
- USPS tracking uses the current Tracking v3.2 ("v3r2") API - a single
  `POST https://apis.usps.com/tracking/v3r2/tracking` call with a JSON array
  body (`[{"trackingNumber": "..."}]`), which replaced the older v3 API's
  `GET /tracking/{trackingNumber}` shape entirely. `statusCategory` in the
  response still works the same way (`"Pre-Shipment"` = no physical scan
  yet, anything else = scanned). It also retries on 429 using the
  `Retry-After` header, the same as Goflow's own rate limiting.
- Lookups are de-duplicated per (carrier, tracking number), so a shipment
  with duplicate boxes/tracking numbers is only looked up once. Carriers run
  concurrently with each other, and each carrier's own queue is also worked
  by several goroutines at once (a small worker pool per carrier), gated by
  a per-carrier token-bucket rate limiter (`rateLimiter` in
  `cmd/tracker/main.go`) so the sustained request rate against that carrier
  stays under its limit even with multiple lookups in flight. Pool size and
  requests/second are set per carrier in `carrierRateLimits` in
  `cmd/tracker/main.go` - they're conservative guesses, not numbers from a
  signed agreement with each carrier, so tune them against your own
  account's actual documented/contracted limits.
- Goflow API access itself lives in `internal/goflow` (its own package, so
  other command binaries in this module - e.g. `cmd/package-level-detail` -
  can import `internal-shipment-tracker/internal/goflow` without pulling in
  the CLI code) rather than in `cmd/tracker/main.go`.
- Carrier selection (the welcome menu's second prompt) is applied before
  the ping pass, not after: a carrier you don't select is dropped from
  consideration immediately, so it never gets pinged, never shows up in the
  ping log, and can never trigger the high-error-rate `ALERT` below either -
  from that carrier's perspective, not selecting it looks identical to
  never having configured its credentials at all.
- Every carrier with credentials configured *and selected on the carrier
  menu* gets an initial ping (an OAuth token request, nothing more) before
  any tracking lookups happen. A
  carrier that fails its ping - bad client ID/secret, expired refresh
  token, that carrier's auth endpoint unreachable - is dropped for the rest
  of the run and treated exactly like a carrier with no credentials at all
  (blank "carrier scanned" column, `missing credentials` on its progress
  line). This only catches credentials that are outright broken, not
  credentials that work but lack authorization for a specific API - see the
  next bullet for that case.
- If a carrier fails almost every one of its lookups (90%+ of at least 5),
  an `*** ALERT: ... ***` block is printed (and logged) calling that out as
  likely an account-level problem - expired/invalid credentials, no API
  access authorization for that account, a changed endpoint - rather than a
  rate limit or a handful of flaky calls, since a rate limit only fails a
  small fraction of calls, not nearly all of them. Check the individual
  "Warning: could not check ..." line(s) for that carrier for the actual
  error. (This is the mechanism that catches a carrier whose credentials
  ping succeeds - valid client ID/secret - but whose account isn't actually
  authorized to call the tracking endpoint itself, e.g. USPS's Tracking API
  Access Controls; the ping above can't see that distinction since it never
  calls the tracking endpoint.)
- The progress bars use ANSI cursor-movement escape codes, so they expect a
  real terminal. If output is redirected to a file/pipe, you'll still get
  correct results, just with those escape codes in the raw output.
- Beyond the per-carrier rate limiter above, no additional backoff is
  implemented for carrier APIs (Goflow's and USPS's own 429s are already
  handled with a retry/backoff loop; UPS/FedEx/Amazon Shipping don't retry
  on 429). For very large date ranges, or if `carrierRateLimits` is tuned
  too aggressively, you may still hit carrier rate limits.