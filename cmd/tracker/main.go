// Command tracker opens with a welcome menu ("Check for carrier scan" or
// "Help me" - the latter prints this usage text and asks again), then asks
// which configured carrier(s) to check (or "All"), then a date range, pulls
// every order that Goflow marked "shipped" whose shipment.shipped_at falls
// in that range, and writes a CSV with one row per tracking number:
//
//	order id, date shipped, carrier, shipping method, tracking number, carrier scanned
//
// "carrier scanned" is "yes" if the carrier's tracking API shows at least one
// physical scan (the package has actually been received into their network),
// and blank if it hasn't been scanned yet, the lookup failed, the carrier
// wasn't selected on the carrier menu, or no tracking API is configured for
// that carrier.
//
// Required environment variables:
//
//	GOFLOW_SUBDOMAIN   the subdomain you use to access Goflow
//	                   (e.g. "companyname" for companyname.goflow.com)
//	GOFLOW_API_TOKEN   a Goflow API token (Settings -> API Tokens)
//
// Optional environment variables, one pair per carrier you want checked.
// Carriers without credentials are simply left blank in the "carrier
// scanned" column - and so is a carrier whose credentials are present but
// fail an initial ping (a plain OAuth token request, logged as
// "<carrier> ping ok"/"ping FAILED"): it's dropped for the rest of that
// run exactly as if it had no credentials at all, rather than spending
// time on tracking lookups that would only fail the same way:
//
//	UPS_CLIENT_ID / UPS_CLIENT_SECRET       UPS OAuth client credentials
//	FEDEX_CLIENT_ID / FEDEX_CLIENT_SECRET   FedEx OAuth client credentials
//	USPS_CLIENT_ID / USPS_CLIENT_SECRET     USPS OAuth client credentials
//
//	AMAZON_SHIPPING_CLIENT_ID       Amazon Shipping (SP-API) LWA client ID
//	AMAZON_SHIPPING_CLIENT_SECRET   Amazon Shipping (SP-API) LWA client secret
//	AMAZON_SHIPPING_REFRESH_TOKEN   Amazon Shipping (SP-API) LWA refresh token
//	AMAZON_SHIPPING_ENDPOINT        optional SP-API regional endpoint override
//	                                (defaults to the North America endpoint)
//
// Amazon Shipping requires all three of AMAZON_SHIPPING_CLIENT_ID,
// AMAZON_SHIPPING_CLIENT_SECRET and AMAZON_SHIPPING_REFRESH_TOKEN - if any
// one of them is missing, Amazon Shipping tracking lookups are skipped just
// like any other carrier without full credentials.
//
// Any of the above may instead be placed in a ".env" file next to this
// program's source (or its compiled binary), one KEY=VALUE per line. Actual
// environment variables always take precedence over the .env file.
//
// Usage:
//
//	go run . [flags]
//	tracker [flags]
//
// Flags:
//
//	-dir string   directory to write the CSV and log file to (default ".",
//	              created if it doesn't already exist)
//	-h, -help     show usage and exit
//
// "help" as the first (and only) argument works the same as -h.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"internal-shipment-tracker/internal/env"
	"internal-shipment-tracker/internal/goflow"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	goflowSubdomain string
	goflowToken     string

	upsClientID     string
	upsClientSecret string

	fedexClientID     string
	fedexClientSecret string

	uspsClientID     string
	uspsClientSecret string

	amazonShippingClientID     string
	amazonShippingClientSecret string
	amazonShippingRefreshToken string
	amazonShippingEndpoint     string
}

func loadConfig() (config, error) {
	cfg := config{
		goflowSubdomain: os.Getenv("GOFLOW_SUBDOMAIN"),
		goflowToken:     os.Getenv("GOFLOW_API_TOKEN"),

		upsClientID:     os.Getenv("UPS_CLIENT_ID"),
		upsClientSecret: os.Getenv("UPS_CLIENT_SECRET"),

		fedexClientID:     os.Getenv("FEDEX_CLIENT_ID"),
		fedexClientSecret: os.Getenv("FEDEX_CLIENT_SECRET"),

		uspsClientID:     os.Getenv("USPS_CLIENT_ID"),
		uspsClientSecret: os.Getenv("USPS_CLIENT_SECRET"),

		amazonShippingClientID:     os.Getenv("AMAZON_SHIPPING_CLIENT_ID"),
		amazonShippingClientSecret: os.Getenv("AMAZON_SHIPPING_CLIENT_SECRET"),
		amazonShippingRefreshToken: os.Getenv("AMAZON_SHIPPING_REFRESH_TOKEN"),
		amazonShippingEndpoint:     os.Getenv("AMAZON_SHIPPING_ENDPOINT"),
	}
	if cfg.goflowSubdomain == "" || cfg.goflowToken == "" {
		return cfg, fmt.Errorf("GOFLOW_SUBDOMAIN and GOFLOW_API_TOKEN environment variables are required")
	}
	if cfg.amazonShippingEndpoint == "" {
		cfg.amazonShippingEndpoint = "https://sellingpartnerapi-na.amazon.com"
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Interactive menu
// ---------------------------------------------------------------------------

// menuCheckCarrierScan and menuHelpMe are the 1-based option numbers on the
// welcome menu main() shows before doing anything else - named instead of
// inlined so the menu's option list and the switch on the user's choice
// can't silently drift out of sync with each other.
const (
	menuCheckCarrierScan = 1
	menuHelpMe           = 2
)

// promptMenu prints title followed by a 1-based numbered list of options,
// asks for a number, and keeps re-prompting (rather than failing) until the
// user enters one that's actually on the list - this is the program's
// front door, so a typo shouldn't need a restart to recover from.
//
// in must be the one *bufio.Reader shared by every interactive prompt in a
// given run (see main()) - each prompt creating its own would risk losing
// whatever the previous prompt's read call had already buffered past the
// line it consumed, silently eating a later prompt's answer.
func promptMenu(in *bufio.Reader, out io.Writer, title string, options []string) (int, error) {
	for {
		fmt.Fprintln(out, title)
		for i, opt := range options {
			fmt.Fprintf(out, "  %d. %s\n", i+1, opt)
		}
		fmt.Fprint(out, "Enter a number: ")

		line, readErr := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" && readErr != nil {
			return 0, fmt.Errorf("failed to read input: %w", readErr)
		}

		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return n, nil
		}
		fmt.Fprintf(out, "Please enter a number between 1 and %d.\n\n", len(options))
	}
}

// promptCarrierSelection asks which of the given carriers (in the order
// passed in) to check this run, alongside an "All" option, and keeps
// re-prompting until it gets a well-formed answer. Accepts one or more
// numbers separated by commas, e.g. "2,3"; selecting "All" (always option
// 1) selects every carrier passed in, regardless of anything else also
// typed alongside it.
//
// in must be the same shared *bufio.Reader described on promptMenu.
func promptCarrierSelection(in *bufio.Reader, out io.Writer, carriers []string) ([]string, error) {
	options := append([]string{"All"}, carriers...)
	for {
		fmt.Fprintln(out, "\nWhich carriers would you like to check?")
		for i, opt := range options {
			fmt.Fprintf(out, "  %d. %s\n", i+1, opt)
		}
		fmt.Fprint(out, `Enter one or more numbers separated by commas (e.g. "2,3"): `)

		line, readErr := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			if readErr != nil {
				return nil, fmt.Errorf("failed to read input: %w", readErr)
			}
			fmt.Fprintln(out, "Please make a selection.")
			continue
		}

		selected := map[int]bool{}
		valid := true
		for _, part := range strings.Split(line, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(options) {
				valid = false
				break
			}
			selected[n] = true
		}
		if !valid {
			fmt.Fprintf(out, "Please enter one or more numbers between 1 and %d, separated by commas.\n\n", len(options))
			continue
		}

		if selected[1] {
			return carriers, nil
		}
		var chosen []string
		for i, carrier := range carriers {
			if selected[i+2] { // options[0] is "All"; carriers[i] is options[i+1]
				chosen = append(chosen, carrier)
			}
		}
		return chosen, nil
	}
}

// ---------------------------------------------------------------------------
// Date range prompt
// ---------------------------------------------------------------------------

// The "to" date is optional - if omitted, promptDateRange fills in today's
// date (UTC) as the end date, so a single date means "from that date
// through now."
var dateRangeRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:\s+to\s+(\d{4}-\d{2}-\d{2}))?$`)

// promptDateRange asks the user for a "YYYY-MM-DD to YYYY-MM-DD" range (or
// just a single "YYYY-MM-DD" start date, which means through today) and
// returns the start and end dates (inclusive), both at midnight UTC.
//
// in must be the same shared *bufio.Reader described on promptMenu.
func promptDateRange(in *bufio.Reader, out io.Writer) (start, end time.Time, err error) {
	fmt.Fprint(out, "Enter date range (YYYY-MM-DD to YYYY-MM-DD, or just YYYY-MM-DD to use today as the end date): ")
	line, readErr := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if readErr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("failed to read input: %w", readErr)
		}
		return time.Time{}, time.Time{}, fmt.Errorf("no input given")
	}

	m := dateRangeRe.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, time.Time{}, fmt.Errorf(`input must look like "2026-01-01 to 2026-01-31" or just "2026-01-01"`)
	}

	start, err = time.ParseInLocation("2006-01-02", m[1], time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid start date: %w", err)
	}

	if m[2] == "" {
		now := time.Now().UTC()
		end = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	} else {
		end, err = time.ParseInLocation("2006-01-02", m[2], time.UTC)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid end date: %w", err)
		}
	}

	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end date must not be before start date")
	}
	return start, end, nil
}

// ---------------------------------------------------------------------------
// Carrier tracking lookups
// ---------------------------------------------------------------------------

// retryAfterDelay parses a Retry-After header value (USPS's Tracking v3.2
// API documents this as the number of seconds to wait before retrying a
// rate-limited request, the same convention Goflow uses - see
// internal/goflow's own copy of this) and rounds it up to the nearest whole
// second. If the header is missing or unparseable, it falls back to a
// conservative 1 second delay.
func retryAfterDelay(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return time.Second
	}
	secs, err := strconv.ParseFloat(header, 64)
	if err != nil || secs < 0 {
		return time.Second
	}
	return time.Duration(int(math.Ceil(secs))) * time.Second
}

// scanChecker reports whether a carrier has recorded at least one physical
// scan of a shipment - i.e. it's not sitting in a "label created / awaiting
// pickup" state, but has actually been received into the carrier's network.
type scanChecker interface {
	// Ping verifies that this checker's credentials actually work, without
	// looking up any particular shipment - each implementation just does
	// its OAuth exchange (the same one Scanned would need anyway) and
	// discards the token. A non-nil error means the credentials are bad
	// (or the carrier's auth endpoint is unreachable), not that any
	// specific tracking number failed.
	Ping(ctx context.Context) error
	Scanned(ctx context.Context, trackingNumber string) (bool, error)
}

// Amazon's "amazon_shipping" (Buy Shipping) service usually just resells a
// label from a real carrier - UPS, USPS, or FedEx - at a negotiated rate.
// When that's the case, the tracking number itself looks exactly like that
// carrier's own format, and can be looked up directly through UPS/FedEx/USPS
// without ever touching Amazon's SP-API (which requires seller-authorized
// credentials). These patterns are deliberately conservative: they only
// match formats that are distinctive enough not to collide with each other.
var (
	upsTrackingNumberRe   = regexp.MustCompile(`^1Z[0-9A-Z]{16}$`)
	fedexTrackingNumberRe = regexp.MustCompile(`^\d{12}$|^\d{15}$`)
	uspsTrackingNumberRe  = regexp.MustCompile(`^\d{20}$|^\d{22}$|^[A-Z]{2}\d{9}US$`)
)

// detectCarrierFromTrackingNumber returns "ups", "fedex", or "usps" if
// trackingNumber's shape unambiguously matches that carrier's own tracking
// number format, or "" if it doesn't match any of them (including if it's
// genuinely an Amazon-only tracking ID).
func detectCarrierFromTrackingNumber(trackingNumber string) string {
	tn := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(trackingNumber), " ", ""))
	switch {
	case upsTrackingNumberRe.MatchString(tn):
		return "ups"
	case uspsTrackingNumberRe.MatchString(tn):
		return "usps"
	case fedexTrackingNumberRe.MatchString(tn):
		return "fedex"
	default:
		return ""
	}
}

// ---- Rate limiting ----------------------------------------------------------

// rateLimiter is a simple token-bucket limiter: up to `burst` requests may
// go through immediately, and after that tokens refill at a steady `rps`
// per second, blocking Wait callers until one is available. This is what
// lets a carrier's queue be worked by several goroutines at once - enough
// to get real concurrency - while still capping the *sustained* request
// rate against that carrier so it doesn't get rate-limited (429s) or worse,
// have its credentials throttled/suspended for abuse.
type rateLimiter struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rps    float64
	last   time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	if rps <= 0 {
		rps = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &rateLimiter{
		tokens: float64(burst),
		max:    float64(burst),
		rps:    rps,
		last:   time.Now(),
	}
}

// Wait blocks until a token is available (refilling the bucket based on
// elapsed time first) or ctx is done, then consumes one token.
func (r *rateLimiter) Wait(ctx context.Context) error {
	for {
		r.mu.Lock()
		now := time.Now()
		r.tokens += now.Sub(r.last).Seconds() * r.rps
		if r.tokens > r.max {
			r.tokens = r.max
		}
		r.last = now

		if r.tokens >= 1 {
			r.tokens--
			r.mu.Unlock()
			return nil
		}

		wait := time.Duration((1 - r.tokens) / r.rps * float64(time.Second))
		r.mu.Unlock()
		if wait <= 0 {
			wait = time.Millisecond
		}

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// carrierRateLimit caps how a single carrier's queue is worked: rps is the
// sustained average request rate to allow against that carrier's tracking
// API, and concurrency is how many of that carrier's lookups may be in
// flight at once (also used as the token bucket's burst size, since that's
// the most tokens that could ever be requested at the same instant).
//
// These numbers are deliberately conservative guesses, not numbers pulled
// from a signed agreement with each carrier - actual limits vary by
// account/contract and change over time. Tune them against your own
// carrier developer portal / API agreement if lookups are getting
// rate-limited (or if there's clearly headroom to go faster).
type carrierRateLimit struct {
	rps         float64
	concurrency int
}

var carrierRateLimits = map[string]carrierRateLimit{
	"ups":             {rps: 5, concurrency: 4},
	"fedex":           {rps: 5, concurrency: 4},
	"usps":            {rps: 5, concurrency: 3},
	"amazon_shipping": {rps: 1, concurrency: 2},
}

// defaultCarrierRateLimit applies to any carrier missing from
// carrierRateLimits (e.g. a new one added per SKILLS.md whose entry was
// forgotten there) - conservative enough not to hammer an API this program
// has no documented limit for.
var defaultCarrierRateLimit = carrierRateLimit{rps: 2, concurrency: 2}

func rateLimitFor(carrier string) carrierRateLimit {
	if l, ok := carrierRateLimits[carrier]; ok {
		return l
	}
	return defaultCarrierRateLimit
}

// ---- UPS -------------------------------------------------------------------

type upsChecker struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func newUPSChecker(clientID, clientSecret string) *upsChecker {
	return &upsChecker{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (u *upsChecker) token(ctx context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.accessToken != "" && time.Now().Before(u.expiresAt) {
		return u.accessToken, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://onlinetools.ups.com/security/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(u.clientID, u.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ups oauth failed (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	secs, _ := strconv.Atoi(parsed.ExpiresIn)
	if secs <= 60 {
		secs = 3600
	}
	u.accessToken = parsed.AccessToken
	u.expiresAt = time.Now().Add(time.Duration(secs-60) * time.Second)
	return u.accessToken, nil
}

// Ping just does the OAuth exchange and throws away the token - if that
// succeeds, the credentials are good.
func (u *upsChecker) Ping(ctx context.Context) error {
	_, err := u.token(ctx)
	return err
}

func (u *upsChecker) Scanned(ctx context.Context, trackingNumber string) (bool, error) {
	tok, err := u.token(ctx)
	if err != nil {
		return false, err
	}

	reqURL := fmt.Sprintf("https://onlinetools.ups.com/api/track/v1/details/%s", url.PathEscape(trackingNumber))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("transId", strconv.FormatInt(time.Now().UnixNano(), 10))
	req.Header.Set("transactionSrc", "tracker")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("ups track failed (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		TrackResponse struct {
			Shipment []struct {
				Package []struct {
					Activity []struct {
						Status struct {
							Type string `json:"type"`
						} `json:"status"`
					} `json:"activity"`
				} `json:"package"`
			} `json:"shipment"`
		} `json:"trackResponse"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, err
	}

	for _, shp := range parsed.TrackResponse.Shipment {
		for _, pkg := range shp.Package {
			for _, act := range pkg.Activity {
				// UPS status type "M" = Manifest: UPS has the electronic
				// shipment record but has NOT yet physically scanned the
				// package. Any other type (e.g. "P" pickup, "I" in transit,
				// "D" delivered, "X" exception) means a physical scan
				// happened. Verify current codes against UPS's Tracking API
				// docs if this ever looks wrong.
				if strings.ToUpper(act.Status.Type) != "M" {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// ---- FedEx ------------------------------------------------------------------

type fedexChecker struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func newFedExChecker(clientID, clientSecret string) *fedexChecker {
	return &fedexChecker{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (f *fedexChecker) token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.accessToken != "" && time.Now().Before(f.expiresAt) {
		return f.accessToken, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", f.clientID)
	form.Set("client_secret", f.clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://apis.fedex.com/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fedex oauth failed (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	secs := parsed.ExpiresIn
	if secs <= 60 {
		secs = 3600
	}
	f.accessToken = parsed.AccessToken
	f.expiresAt = time.Now().Add(time.Duration(secs-60) * time.Second)
	return f.accessToken, nil
}

// Ping just does the OAuth exchange and throws away the token - if that
// succeeds, the credentials are good.
func (f *fedexChecker) Ping(ctx context.Context) error {
	_, err := f.token(ctx)
	return err
}

func (f *fedexChecker) Scanned(ctx context.Context, trackingNumber string) (bool, error) {
	tok, err := f.token(ctx)
	if err != nil {
		return false, err
	}

	payload := map[string]interface{}{
		"includeDetailedScans": true,
		"trackingInfo": []map[string]interface{}{
			{
				"trackingNumberInfo": map[string]string{
					"trackingNumber": trackingNumber,
				},
			},
		},
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://apis.fedex.com/track/v1/trackingnumbers", bytes.NewReader(bodyBytes))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-locale", "en_US")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("fedex track failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		Output struct {
			CompleteTrackResults []struct {
				TrackResults []struct {
					ScanEvents []struct {
						DerivedStatusCode string `json:"derivedStatusCode"`
						EventType         string `json:"eventType"`
					} `json:"scanEvents"`
				} `json:"trackResults"`
			} `json:"completeTrackResults"`
		} `json:"output"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return false, err
	}

	for _, ctr := range parsed.Output.CompleteTrackResults {
		for _, tr := range ctr.TrackResults {
			for _, ev := range tr.ScanEvents {
				code := strings.ToUpper(ev.EventType)
				if code == "" {
					code = strings.ToUpper(ev.DerivedStatusCode)
				}
				// "OC" = Order Created: FedEx has the electronic label but
				// hasn't physically received the package yet. Any other
				// scan code (PU pickup, AR arrived, DP departed, IT in
				// transit, OD out for delivery, DL delivered, ...) means a
				// physical scan happened. Verify current codes against
				// FedEx's Track API docs if this ever looks wrong.
				if code != "" && code != "OC" {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// ---- USPS -------------------------------------------------------------------

type uspsChecker struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func newUSPSChecker(clientID, clientSecret string) *uspsChecker {
	return &uspsChecker{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (u *uspsChecker) token(ctx context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.accessToken != "" && time.Now().Before(u.expiresAt) {
		return u.accessToken, nil
	}

	payload := map[string]string{
		"client_id":     u.clientID,
		"client_secret": u.clientSecret,
		"grant_type":    "client_credentials",
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://apis.usps.com/oauth2/v3/token", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("usps oauth failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", err
	}
	secs := parsed.ExpiresIn
	if secs <= 60 {
		secs = 28800
	}
	u.accessToken = parsed.AccessToken
	u.expiresAt = time.Now().Add(time.Duration(secs-60) * time.Second)
	return u.accessToken, nil
}

// Ping just does the OAuth exchange and throws away the token - if that
// succeeds, the credentials are good.
func (u *uspsChecker) Ping(ctx context.Context) error {
	_, err := u.token(ctx)
	return err
}

// Scanned uses USPS's Tracking v3.2 (v3r2) API: a single POST /tracking
// call whose body and response are both JSON arrays (USPS supports batching
// up to 35 tracking numbers per call - this always sends just one). This
// replaced the older v3 API's "GET /tracking/{trackingNumber}" shape
// entirely; see the v3r2 OpenAPI spec for the current schema.
func (u *uspsChecker) Scanned(ctx context.Context, trackingNumber string) (bool, error) {
	tok, err := u.token(ctx)
	if err != nil {
		return false, err
	}

	payload := []map[string]string{
		{"trackingNumber": trackingNumber},
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://apis.usps.com/tracking/v3r2/tracking", bytes.NewReader(bodyBytes))
		if err != nil {
			return false, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := u.httpClient.Do(req)
		if err != nil {
			return false, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return false, readErr
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			// The v3r2 spec documents a Retry-After header (seconds) on 429s,
			// same convention as Goflow's own rate limiting.
			select {
			case <-time.After(retryAfterDelay(resp.Header.Get("Retry-After"))):
			case <-ctx.Done():
				return false, ctx.Err()
			}
			continue
		}

		// 200 returns a TrackingDetails array; a batch call can also return
		// 207 (MultiStatusResponse) where each element is either a success
		// (tracking detail, statusCode "200") or a failure (statusCode
		// "404" plus error info) - since exactly one tracking number was
		// requested, at most one element comes back either way.
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusMultiStatus {
			return false, fmt.Errorf("usps track failed (%d): %s", resp.StatusCode, string(body))
		}

		var results []struct {
			StatusCode     string `json:"statusCode"`
			StatusCategory string `json:"statusCategory"`
			Error          *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &results); err != nil {
			return false, err
		}
		if len(results) == 0 {
			return false, fmt.Errorf("usps returned no tracking result for %s", trackingNumber)
		}

		r := results[0]
		if r.StatusCode != "" && r.StatusCode != "200" {
			msg := r.StatusCode
			if r.Error != nil && r.Error.Message != "" {
				msg = r.Error.Message
			}
			return false, fmt.Errorf("usps could not track %s: %s", trackingNumber, msg)
		}

		// USPS explicitly labels shipments that only have an electronic label
		// (no physical acceptance scan yet) with statusCategory "Pre-Shipment".
		// Anything else (In Transit, Out for Delivery, Delivered, Available for
		// Pickup, etc.) means USPS has physically scanned the package at least
		// once. Verify against USPS's Tracking v3.2 docs if this ever looks wrong.
		return !strings.EqualFold(r.StatusCategory, "Pre-Shipment"), nil
	}
}

// ---- Amazon Shipping ---------------------------------------------------------

// amazonShippingChecker calls Amazon's Selling Partner API (SP-API) Shipping
// service to check tracking for the "amazon_shipping" carrier (Amazon's Buy
// Shipping / label service - distinct from "amazon_logistics", Amazon's own
// last-mile delivery network, which isn't handled here).
//
// Unlike UPS/FedEx/USPS, SP-API access tokens come from a refresh_token
// grant (Login With Amazon), not client_credentials, because access has to
// be tied to a specific seller's authorization - hence the extra
// AMAZON_SHIPPING_REFRESH_TOKEN requirement.
type amazonShippingChecker struct {
	clientID     string
	clientSecret string
	refreshToken string
	endpoint     string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func newAmazonShippingChecker(clientID, clientSecret, refreshToken, endpoint string) *amazonShippingChecker {
	return &amazonShippingChecker{
		clientID:     clientID,
		clientSecret: clientSecret,
		refreshToken: refreshToken,
		endpoint:     endpoint,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (a *amazonShippingChecker) token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.accessToken != "" && time.Now().Before(a.expiresAt) {
		return a.accessToken, nil
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", a.refreshToken)
	form.Set("client_id", a.clientID)
	form.Set("client_secret", a.clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.amazon.com/auth/o2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("amazon shipping oauth failed (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	secs := parsed.ExpiresIn
	if secs <= 60 {
		secs = 3600
	}
	a.accessToken = parsed.AccessToken
	a.expiresAt = time.Now().Add(time.Duration(secs-60) * time.Second)
	return a.accessToken, nil
}

// Ping just does the OAuth exchange and throws away the token - if that
// succeeds, the credentials are good.
func (a *amazonShippingChecker) Ping(ctx context.Context) error {
	_, err := a.token(ctx)
	return err
}

func (a *amazonShippingChecker) Scanned(ctx context.Context, trackingNumber string) (bool, error) {
	tok, err := a.token(ctx)
	if err != nil {
		return false, err
	}

	reqURL := fmt.Sprintf("%s/shipping/v2/tracking/%s", strings.TrimRight(a.endpoint, "/"), url.PathEscape(trackingNumber))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, err
	}
	// SP-API operations that aren't in the "restricted data" category
	// (tracking info is not) only need the LWA access token, not a signed
	// AWS request. If Amazon's Shipping API rejects this for your
	// application type, this call will need AWS SigV4 signing added.
	req.Header.Set("x-amz-access-token", tok)
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("amazon shipping track failed (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Payload struct {
			Summary struct {
				Status string `json:"status"`
			} `json:"summary"`
			EventHistory []struct {
				EventCode string `json:"eventCode"`
			} `json:"eventHistory"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, err
	}

	// Any recorded tracking event means the package was physically scanned
	// at least once.
	for _, ev := range parsed.Payload.EventHistory {
		if ev.EventCode != "" {
			return true, nil
		}
	}
	// Fall back to the overall status: "Unknown"/"LabelCreated" (naming per
	// Amazon's Shipping API docs) mean only an electronic label exists with
	// no physical scan yet; anything else (PickedUp, InTransit,
	// OutForDelivery, Delivered, ...) means a scan happened. Verify against
	// Amazon's current SP-API Shipping docs if this ever looks wrong.
	status := strings.ToLower(parsed.Payload.Summary.Status)
	return status != "" && status != "unknown" && status != "labelcreated", nil
}

// ---------------------------------------------------------------------------
// Console progress bars
// ---------------------------------------------------------------------------

// multiProgress renders several independent progress lines in the terminal
// and lets any of them be updated in place afterward, using ANSI cursor
// movement. This is what lets each carrier's tracking lookups run
// concurrently while still showing its own live progress bar.
//
// It assumes the terminal supports ANSI escape codes and that nothing else
// writes to stdout while it's active (carrier lookup errors are collected
// and printed only after all bars are done, for exactly this reason).
type multiProgress struct {
	mu    sync.Mutex
	out   io.Writer
	lines int
}

// newMultiProgress prints one line of initial content per entry in
// initialLines (top to bottom) and returns a handle that can later rewrite
// any of those lines in place, addressed by their 0-based index from the top.
func newMultiProgress(out io.Writer, initialLines []string) *multiProgress {
	for _, line := range initialLines {
		fmt.Fprintln(out, line)
	}
	return &multiProgress{out: out, lines: len(initialLines)}
}

// update overwrites the content of a single line without disturbing the
// others. Safe to call concurrently from multiple goroutines.
func (mp *multiProgress) update(index int, text string) {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	up := mp.lines - index
	fmt.Fprintf(mp.out, "\x1b[%dA\r\x1b[K%s\r\x1b[%dB", up, text, up)
}

const progressBarWidth = 24

// renderProgressBar formats a single "carrier [====----] done/total" line.
func renderProgressBar(carrier string, done, total int) string {
	filled := 0
	if total > 0 {
		filled = progressBarWidth * done / total
		if filled > progressBarWidth {
			filled = progressBarWidth
		}
	}
	bar := strings.Repeat("=", filled) + strings.Repeat(" ", progressBarWidth-filled)
	return fmt.Sprintf("  %-16s [%s] %d/%d", carrier, bar, done, total)
}

// renderProgressText formats a status line for a carrier that has no
// progress bar to show (e.g. missing credentials), using the exact same
// column layout as renderProgressBar - the bar itself is replaced with
// text (truncated and padded to the same width), and the count still shows
// on the right, so both kinds of lines stay visually aligned.
func renderProgressText(carrier, text string, done, total int) string {
	if len(text) > progressBarWidth {
		text = text[:progressBarWidth]
	}
	text += strings.Repeat(" ", progressBarWidth-len(text))
	return fmt.Sprintf("  %-16s [%s] %d/%d", carrier, text, done, total)
}

// ---------------------------------------------------------------------------
// Log file
// ---------------------------------------------------------------------------

// dualWriter returns a writer that sends everything to primary (normally
// os.Stdout or os.Stderr) and, when logFile is non-nil, also to logFile -
// so console output and the "logs_shipped_orders_*.txt" file it's mirrored
// into always agree. If logFile is nil (nothing to log to yet, e.g. before
// the date range is known), it just returns primary unchanged.
func dualWriter(primary io.Writer, logFile *os.File) io.Writer {
	if logFile == nil {
		return primary
	}
	return io.MultiWriter(primary, logFile)
}

// formatDuration renders a duration for the timing breakdown: whole
// milliseconds under 1000ms, seconds (one decimal place) from there up to
// 120 sec, and minutes (one decimal place) beyond that (e.g. "420 ms",
// "2.3 sec", "3.5 min").
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < 120*time.Second:
		return fmt.Sprintf("%.1f sec", d.Seconds())
	default:
		return fmt.Sprintf("%.1f min", d.Minutes())
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

type reportRow struct {
	orderID        int64
	dateShipped    string
	carrier        string
	shippingMethod string
	tracking       string
	scanned        string
}

// printUsage writes the program's help text to out. It's used both for
// "-h"/"-help" (via flag.Usage) and for a plain "help" argument.
func printUsage(out io.Writer) {
	fmt.Fprintln(out, "tracker")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Pulls Goflow orders marked \"shipped\" in a date range, checks carrier")
	fmt.Fprintln(out, "tracking APIs for scan status, and writes a CSV plus a matching log file.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Running it with no arguments opens an interactive menu: choose which")
	fmt.Fprintln(out, "carrier(s) to check (or \"All\"), then enter a date range, and it runs as")
	fmt.Fprintln(out, "described above.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  tracker [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags:")
	flag.CommandLine.SetOutput(out)
	flag.PrintDefaults()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Configuration (environment variables, or a \".env\" file next to this")
	fmt.Fprintln(out, "program - see README.md / .env.example for details):")
	fmt.Fprintln(out, "  GOFLOW_SUBDOMAIN, GOFLOW_API_TOKEN                            required")
	fmt.Fprintln(out, "  UPS_CLIENT_ID / UPS_CLIENT_SECRET                             optional")
	fmt.Fprintln(out, "  FEDEX_CLIENT_ID / FEDEX_CLIENT_SECRET                         optional")
	fmt.Fprintln(out, "  USPS_CLIENT_ID / USPS_CLIENT_SECRET                           optional")
	fmt.Fprintln(out, "  AMAZON_SHIPPING_CLIENT_ID / _CLIENT_SECRET / _REFRESH_TOKEN   optional")
	fmt.Fprintln(out, "  AMAZON_SHIPPING_ENDPOINT                                      optional")
}

func main() {
	programStart := time.Now()

	var outDir string
	flag.StringVar(&outDir, "dir", ".", "directory to write the CSV and log file to (created if it doesn't exist)")
	flag.Usage = func() { printUsage(os.Stderr) }
	flag.Parse()

	if flag.Arg(0) == "help" {
		printUsage(os.Stdout)
		os.Exit(0)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "Error creating output directory:", err)
		os.Exit(1)
	}

	if err := env.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	// Only build a checker for a carrier when ALL of its required
	// credentials are present; a partial credential set is treated the same
	// as no credentials at all, so no tracking lookup is ever attempted for
	// it.
	checkers := map[string]scanChecker{}
	if cfg.upsClientID != "" && cfg.upsClientSecret != "" {
		checkers["ups"] = newUPSChecker(cfg.upsClientID, cfg.upsClientSecret)
	}
	if cfg.fedexClientID != "" && cfg.fedexClientSecret != "" {
		checkers["fedex"] = newFedExChecker(cfg.fedexClientID, cfg.fedexClientSecret)
	}
	if cfg.uspsClientID != "" && cfg.uspsClientSecret != "" {
		checkers["usps"] = newUSPSChecker(cfg.uspsClientID, cfg.uspsClientSecret)
	}
	if cfg.amazonShippingClientID != "" && cfg.amazonShippingClientSecret != "" && cfg.amazonShippingRefreshToken != "" {
		checkers["amazon_shipping"] = newAmazonShippingChecker(
			cfg.amazonShippingClientID, cfg.amazonShippingClientSecret, cfg.amazonShippingRefreshToken, cfg.amazonShippingEndpoint)
	}
	// knownCarriers fixes the display order for the progress bars below and
	// is also the full list of carriers this program knows how to check at
	// all; anything else Goflow reports is simply left blank.
	knownCarriers := []string{"ups", "fedex", "usps", "amazon_shipping"}

	// All of this run's interactive prompts (the welcome menu, carrier
	// selection, and the date range below) share this one *bufio.Reader
	// rather than each wrapping os.Stdin freshly - see promptMenu's doc
	// comment for why that matters.
	stdin := bufio.NewReader(os.Stdin)

	// Welcome menu: loop on "Help me" (print usage, then ask again) until
	// the user picks "Check for carrier scan" - that's the only thing this
	// program actually does, but "Help me" gives a way to see the man page
	// without already knowing about -h/-help/a bare "help" argument.
	for {
		choice, err := promptMenu(stdin, os.Stdout, "Welcome! What would you like to do?",
			[]string{"Check for carrier scan", "Help me"})
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if choice == menuHelpMe {
			fmt.Println()
			printUsage(os.Stdout)
			fmt.Println()
			continue
		}
		break // menuCheckCarrierScan
	}

	// Ask which carriers to check, offered only from those with credentials
	// configured (in the fixed knownCarriers order) plus "All". Deselected
	// carriers are dropped from checkers right away, so everything
	// downstream (the ping pass, progress bars, pass 1, tallies) treats
	// them exactly like a carrier with no credentials at all - the same
	// trick the ping pass itself already relies on.
	var availableCarriers []string
	for _, carrier := range knownCarriers {
		if _, ok := checkers[carrier]; ok {
			availableCarriers = append(availableCarriers, carrier)
		}
	}
	if len(availableCarriers) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no carrier credentials are configured (UPS/FedEx/USPS/Amazon Shipping) - nothing to check.")
		os.Exit(1)
	}
	selectedCarriers, err := promptCarrierSelection(stdin, os.Stdout, availableCarriers)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	selected := make(map[string]bool, len(selectedCarriers))
	for _, carrier := range selectedCarriers {
		selected[carrier] = true
	}
	for carrier := range checkers {
		if !selected[carrier] {
			delete(checkers, carrier)
		}
	}

	start, end, err := promptDateRange(stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	endExclusive := end.Add(24 * time.Hour)

	// Everything from here on is mirrored into a log file alongside the
	// CSV, so a user hitting a problem can just send over
	// logs_shipped_orders_<start>_<end>.txt and it'll show what happened -
	// including any errors that scrolled past in the console. Both files
	// go into -dir (the current directory by default).
	logPath := filepath.Join(outDir, fmt.Sprintf("logs_shipped_orders_%s_%s.txt", start.Format("2006-01-02"), end.Format("2006-01-02")))
	logFile, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error creating log file:", err)
		os.Exit(1)
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "tracker log\nDate range: %s to %s\nStarted: %s\n\n",
		start.Format("2006-01-02"), end.Format("2006-01-02"), time.Now().UTC().Format(time.RFC3339))

	ctx := context.Background()

	// Ping every carrier that has credentials configured, to check those
	// credentials actually work before spending time on tracking lookups.
	// A carrier that fails its ping is dropped from checkers entirely -
	// from here on it's treated exactly like a carrier with no credentials
	// at all, since everything downstream (progress bars, pass 1, tallies)
	// keys off whether checkers[carrier] exists.
	fmt.Fprintln(dualWriter(os.Stdout, logFile), "Pinging carrier APIs to verify credentials...")
	for _, carrier := range knownCarriers {
		checker, ok := checkers[carrier]
		if !ok {
			continue
		}
		if err := checker.Ping(ctx); err != nil {
			fmt.Fprintf(dualWriter(os.Stdout, logFile), "  %-16s ping FAILED - skipping this carrier for the rest of this run: %v\n", carrier, err)
			delete(checkers, carrier)
			continue
		}
		fmt.Fprintf(dualWriter(os.Stdout, logFile), "  %-16s ping ok\n", carrier)
	}

	fmt.Fprintf(dualWriter(os.Stdout, logFile), "Fetching shipped orders from %s to %s...\n", start.Format("2006-01-02"), end.Format("2006-01-02"))
	goflowClient := goflow.NewClient(cfg.goflowSubdomain, cfg.goflowToken)
	goflowClient.Notice = dualWriter(os.Stdout, logFile)
	fetchStart := time.Now()
	orders, err := goflowClient.FetchShippedOrders(ctx, start, endExclusive)
	fetchDuration := time.Since(fetchStart)
	if err != nil {
		fmt.Fprintln(dualWriter(os.Stderr, logFile), "Error fetching orders from Goflow:", err)
		logFile.Close()
		os.Exit(1)
	}
	fmt.Fprintf(dualWriter(os.Stdout, logFile), "Found %d shipped order(s) (before filtering by exact ship date).\n", len(orders))

	type cacheKey struct {
		carrier  string
		tracking string
	}

	// pendingRow is a row waiting on a tracking lookup (or not needing one).
	// lookupCarrier is the carrier whose API will actually be queried for
	// this tracking number - it's usually the same as reportedCarrier, but
	// can differ for "amazon_shipping" (see detectCarrierFromTrackingNumber).
	// It's left empty when no lookup will happen at all (no tracking number,
	// or the reported carrier isn't one this program knows how to check).
	type pendingRow struct {
		orderID         int64
		dateShipped     string
		reportedCarrier string
		shippingMethod  string
		tracking        string
		lookupCarrier   string
	}

	var pending []pendingRow
	queues := map[string][]string{} // lookupCarrier -> unique tracking numbers to check
	queued := map[cacheKey]bool{}   // dedupe within queues
	warned := map[string]bool{}
	fallbackNoted := map[string]bool{}

	// Pass 1: walk every order, decide (without making any network calls
	// yet) which carrier - if any - should be asked about each tracking
	// number, and build the de-duplicated per-carrier work queues.
	for _, o := range orders {
		if o.Shipment == nil || o.Shipment.ShippedAt == nil {
			continue
		}
		shippedAt := o.Shipment.ShippedAt.UTC()
		if shippedAt.Before(start) || !shippedAt.Before(endExclusive) {
			// status_updated_at fell in range, but the actual shipped_at
			// timestamp doesn't (Goflow has no direct shipped_at filter) -
			// skip it so the report only contains what was actually asked for.
			continue
		}

		carrier := ""
		if o.Shipment.Carrier != nil {
			carrier = *o.Shipment.Carrier
		}
		shippingMethod := ""
		if o.Shipment.ShippingMethod != nil {
			shippingMethod = *o.Shipment.ShippingMethod
		}
		dateShipped := shippedAt.Format("2006-01-02")

		var trackingNumbers []string
		for _, b := range o.Shipment.Boxes {
			if b.TrackingNumber != nil {
				if tn := strings.TrimSpace(*b.TrackingNumber); tn != "" {
					trackingNumbers = append(trackingNumbers, tn)
				}
			}
		}

		if len(trackingNumbers) == 0 {
			pending = append(pending, pendingRow{orderID: o.ID, dateShipped: dateShipped, reportedCarrier: carrier, shippingMethod: shippingMethod})
			continue
		}

		for _, tn := range trackingNumbers {
			// Prefer looking up the underlying carrier directly when the
			// tracking number's own format gives it away (mainly
			// "amazon_shipping" labels, which are often just a relabeled
			// UPS/USPS/FedEx shipment) - that avoids needing Amazon SP-API
			// authorization at all for those.
			lookupCarrier := carrier
			_, supported := checkers[lookupCarrier]
			if carrier == "amazon_shipping" {
				if detected := detectCarrierFromTrackingNumber(tn); detected != "" {
					if _, dcOK := checkers[detected]; dcOK {
						lookupCarrier, supported = detected, true
						if !fallbackNoted[detected] {
							fmt.Fprintf(dualWriter(os.Stdout, logFile), "Note: some amazon_shipping tracking numbers look like %s numbers; checking those via the %s tracking API instead of Amazon SP-API.\n", detected, detected)
							fallbackNoted[detected] = true
						}
					}
				}
			}

			// Count this tracking number toward lookupCarrier's queue
			// regardless of whether it's actually supported, so the startup
			// display can show "0/<total>" for a carrier that's missing
			// credentials instead of just a bare "missing credentials" with
			// no indication of how many rows that leaves without results.
			dedupeKey := cacheKey{carrier: lookupCarrier, tracking: tn}
			if !queued[dedupeKey] {
				queued[dedupeKey] = true
				queues[lookupCarrier] = append(queues[lookupCarrier], tn)
			}

			row := pendingRow{orderID: o.ID, dateShipped: dateShipped, reportedCarrier: carrier, shippingMethod: shippingMethod, tracking: tn}
			if !supported {
				if !warned[carrier] {
					label := carrier
					if label == "" {
						label = "(unknown)"
					}
					fmt.Fprintf(dualWriter(os.Stderr, logFile), "Note: no tracking API configured for carrier %q - \"carrier scanned\" will be left blank for it.\n", label)
					warned[carrier] = true
				}
				pending = append(pending, row)
				continue
			}

			row.lookupCarrier = lookupCarrier
			pending = append(pending, row)
		}
	}

	// Build the initial progress display: one line per known carrier,
	// either a live progress bar (credentials found and there's work to
	// do) or a same-width line with "missing credentials" (or "no tracking
	// numbers to check") in place of the bar - tracking numbers are still
	// counted either way, so the total on the right stays visible and every
	// line lines up in the same columns.
	lineIndex := make(map[string]int, len(knownCarriers))
	initialLines := make([]string, 0, len(knownCarriers))
	for _, carrier := range knownCarriers {
		lineIndex[carrier] = len(initialLines)
		total := len(queues[carrier])
		if _, ok := checkers[carrier]; !ok {
			initialLines = append(initialLines, renderProgressText(carrier, " missing credentials", 0, total))
			continue
		}
		if total == 0 {
			initialLines = append(initialLines, renderProgressText(carrier, "no tracking numbers", 0, total))
			continue
		}
		initialLines = append(initialLines, renderProgressBar(carrier, 0, total))
	}

	// The live progress bars use ANSI cursor movement, which would just be
	// noise in a text log file, so log a plain-text snapshot of the
	// starting state once instead (the final per-carrier tallies are
	// logged after the lookups finish, below).
	fmt.Fprintln(logFile, "Carrier tracking lookups:")
	for _, line := range initialLines {
		fmt.Fprintln(logFile, line)
	}

	type scanResult struct {
		scanned bool
		err     error
	}
	// Each carrier gets its own results map. Within a carrier, several
	// worker goroutines now write to the same map concurrently (see below),
	// so each map is paired with its own mutex in workerResults.
	resultsByCarrier := make(map[string]map[string]scanResult, len(queues))
	for carrier, tns := range queues {
		resultsByCarrier[carrier] = make(map[string]scanResult, len(tns))
	}

	fmt.Println("Checking carrier tracking APIs (running several lookups per carrier in parallel, rate-limited per carrier):")
	fmt.Fprintln(logFile, "\nChecking carrier tracking APIs (running several lookups per carrier in parallel, rate-limited per carrier)...")
	mp := newMultiProgress(os.Stdout, initialLines)

	// carrierDurations holds how long each carrier's whole queue took to
	// look up, indexed the same way as lineIndex. Each goroutine below only
	// ever writes to its own index, so this needs no locking - concurrent
	// writes to distinct slice elements are safe.
	carrierDurations := make([]time.Duration, len(knownCarriers))

	var wg sync.WaitGroup
	for _, carrier := range knownCarriers {
		tns := queues[carrier]
		checker, ok := checkers[carrier]
		if !ok || len(tns) == 0 {
			continue
		}
		idx := lineIndex[carrier]
		results := resultsByCarrier[carrier]
		limit := rateLimitFor(carrier)
		limiter := newRateLimiter(limit.rps, limit.concurrency)
		wg.Add(1)
		go func(carrier string, tns []string, checker scanChecker, idx int, results map[string]scanResult, limiter *rateLimiter, concurrency int) {
			defer wg.Done()
			carrierStart := time.Now()

			// Hand tracking numbers out to `concurrency` workers over a
			// channel; each worker waits on the shared limiter before every
			// API call, so the carrier's overall request rate stays capped
			// no matter how many workers are running. done is bumped
			// atomically since every worker updates the same progress line.
			work := make(chan string)
			var resultsMu sync.Mutex
			var done int32

			var workers sync.WaitGroup
			for i := 0; i < concurrency; i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for tn := range work {
						isScanned, err := false, limiter.Wait(ctx)
						if err == nil {
							isScanned, err = checker.Scanned(ctx, tn)
						}

						resultsMu.Lock()
						results[tn] = scanResult{scanned: isScanned, err: err}
						resultsMu.Unlock()

						n := atomic.AddInt32(&done, 1)
						mp.update(idx, renderProgressBar(carrier, int(n), len(tns)))
					}
				}()
			}
			for _, tn := range tns {
				work <- tn
			}
			close(work)
			workers.Wait()

			carrierDurations[idx] = time.Since(carrierStart)
		}(carrier, tns, checker, idx, results, limiter, limit.concurrency)
	}
	wg.Wait()

	// highErrorRateThreshold and minLookupsForHighErrorRateAlert control the
	// "this carrier looks broken" alert below: if a carrier's error rate is
	// at or above the threshold, over a large enough sample to rule out
	// ordinary transient flakiness, that's not a rate limit or a handful of
	// bad tracking numbers - a rate limit or a few bad lookups fails a
	// small fraction of calls, not nearly all of them. Almost total failure
	// means something systemic (bad/expired credentials, no API access
	// authorization, a changed endpoint, ...) that won't go away on its own
	// on the next run, so it's worth surfacing loudly instead of leaving it
	// to blend into a wall of per-tracking-number warnings.
	const (
		highErrorRateThreshold          = 0.9
		minLookupsForHighErrorRateAlert = 5
	)

	// Print any lookup errors only now that every bar has settled, so they
	// don't get interleaved with the in-place progress updates above. These
	// also go to the log file, since they're the main thing worth reporting
	// back if something went wrong.
	fmt.Fprintln(logFile, "\nCarrier tracking lookup results:")
	for _, carrier := range knownCarriers {
		tns := queues[carrier]
		if len(tns) == 0 {
			continue
		}
		if _, ok := checkers[carrier]; !ok {
			// Counted for the "0/<total>" display above, but never actually
			// queried - missing credentials, not a lookup outcome.
			fmt.Fprintf(logFile, "  %-16s missing credentials - %d tracking number(s) left unchecked\n", carrier, len(tns))
			continue
		}
		errCount := 0
		for _, tn := range tns {
			if res := resultsByCarrier[carrier][tn]; res.err != nil {
				errCount++
				fmt.Fprintf(dualWriter(os.Stderr, logFile), "Warning: could not check %s tracking %s: %v\n", carrier, tn, res.err)
			}
		}
		fmt.Fprintf(logFile, "  %-16s checked %d/%d tracking number(s), %d error(s)\n", carrier, len(tns), len(tns), errCount)

		if errRate := float64(errCount) / float64(len(tns)); len(tns) >= minLookupsForHighErrorRateAlert && errRate >= highErrorRateThreshold {
			fmt.Fprintf(dualWriter(os.Stderr, logFile),
				"\n*** ALERT: %s failed %d/%d (%.0f%%) tracking lookups. This looks like an\n"+
					"    account-level problem (expired/invalid credentials, no API access\n"+
					"    authorization for this account, a changed endpoint, ...) rather than a\n"+
					"    rate limit or a few flaky calls - see the \"Warning: could not check\"\n"+
					"    line(s) above for %s for the actual error before re-running. ***\n\n",
				carrier, errCount, len(tns), errRate*100, carrier)
		}
	}

	// Pass 2: turn the pending rows into final report rows now that every
	// carrier's lookups have finished.
	rows := make([]reportRow, 0, len(pending))
	for _, p := range pending {
		scanned := ""
		if p.lookupCarrier != "" {
			if res, ok := resultsByCarrier[p.lookupCarrier][p.tracking]; ok && res.err == nil && res.scanned {
				scanned = "yes"
			}
		}
		rows = append(rows, reportRow{
			orderID:        p.orderID,
			dateShipped:    p.dateShipped,
			carrier:        p.reportedCarrier,
			shippingMethod: p.shippingMethod,
			tracking:       p.tracking,
			scanned:        scanned,
		})
	}

	writeStart := time.Now()
	csvPath := filepath.Join(outDir, fmt.Sprintf("shipped_orders_%s_to_%s.csv", start.Format("2006-01-02"), end.Format("2006-01-02")))
	f, err := os.Create(csvPath)
	if err != nil {
		fmt.Fprintln(dualWriter(os.Stderr, logFile), "Error creating CSV file:", err)
		logFile.Close()
		os.Exit(1)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	_ = w.Write([]string{"order id", "date shipped", "carrier", "shipping method", "tracking number", "carrier scanned"})
	for _, r := range rows {
		_ = w.Write([]string{
			strconv.FormatInt(r.orderID, 10),
			r.dateShipped,
			r.carrier,
			r.shippingMethod,
			r.tracking,
			r.scanned,
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Fprintln(dualWriter(os.Stderr, logFile), "Error writing CSV file:", err)
		logFile.Close()
		os.Exit(1)
	}

	writeDuration := time.Since(writeStart)

	fmt.Fprintf(dualWriter(os.Stdout, logFile), "Wrote %d row(s) to %s\n", len(rows), csvPath)

	// Timing breakdown: total wall time plus each of the major phases, so
	// it's obvious where the time actually went (a slow Goflow fetch vs. a
	// slow carrier API vs. just a lot of rows to write).
	out := dualWriter(os.Stdout, logFile)
	fmt.Fprintln(out, "\nTiming breakdown:")
	fmt.Fprintf(out, "  %-24s %s\n", "Fetching Goflow orders", formatDuration(fetchDuration))
	fmt.Fprintln(out, "  Carrier lookups:")
	for _, carrier := range knownCarriers {
		d := carrierDurations[lineIndex[carrier]]
		if d == 0 {
			continue
		}
		fmt.Fprintf(out, "    %-22s %s\n", carrier, formatDuration(d))
	}
	fmt.Fprintf(out, "  %-24s %s\n", "Writing files", formatDuration(writeDuration))
	fmt.Fprintf(out, "  %-24s %s\n", "Total", formatDuration(time.Since(programStart)))

	fmt.Fprintf(logFile, "\nFinished: %s\n", time.Now().UTC().Format(time.RFC3339))
}
