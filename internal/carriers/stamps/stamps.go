// Package stamps checks Stamps.com/Endicia's SERA REST API to see whether
// a USPS shipment has been physically scanned yet (not just labeled). SERA
// is a reseller/consolidator API that stands in for the native USPS
// Tracking API when configured - see cmd/tracker's checkers map
// construction for the precedence rule between the two.
package stamps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultRedirectURI is used when no STAMPS_REDIRECT_URI is configured.
// Whatever value is actually used - default or overridden - must exactly
// match a redirect URI registered for the client ID in the Stamps.com/
// Endicia developer console (Settings -> Applications); SERA rejects an
// unregistered one.
const defaultRedirectURI = "http://localhost:8080/callback"

// Checker calls Stamps.com/Endicia's SERA API. Like Amazon's SP-API, SERA
// access tokens come from a refresh_token grant, not client_credentials -
// see https://developer.stamps.com/ ("Connect and Authenticate"): a
// refresh_token normally has to be obtained once via the authorization_code
// login flow, then used indefinitely to mint new access tokens.
//
// If no refresh token is supplied to New, Checker obtains one itself on
// first use (see authorizeLocked): it starts a temporary local HTTP server,
// prints a login URL for the user to open, waits for the resulting
// authorization code, and exchanges it for an access_token + refresh_token.
// Both are cached in memory only, for the lifetime of this process - never
// written to disk. Re-running the program means logging in again unless
// the printed refresh_token is saved to STAMPS_REFRESH_TOKEN by hand.
type Checker struct {
	clientID     string
	clientSecret string
	redirectURI  string
	httpClient   *http.Client

	mu           sync.Mutex
	refreshToken string
	accessToken  string
	expiresAt    time.Time
}

// New returns a Checker for the given SERA OAuth client ID/secret
// (production environment - https://signin.stampsendicia.com /
// https://api.stampsendicia.com/sera; SERA's separate staging environment
// isn't wired up here).
//
// refreshToken may be empty - if so, the first call that needs a token
// (Ping or Scanned) triggers the interactive login flow described on
// Checker's doc comment, using redirectURI (or defaultRedirectURI if
// redirectURI is also empty).
func New(clientID, clientSecret, refreshToken, redirectURI string) *Checker {
	return &Checker{
		clientID:     clientID,
		clientSecret: clientSecret,
		refreshToken: refreshToken,
		redirectURI:  redirectURI,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Checker) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Now().Before(c.expiresAt) {
		return c.accessToken, nil
	}

	if c.refreshToken == "" {
		if err := c.authorizeLocked(ctx); err != nil {
			return "", err
		}
		// authorizeLocked already populated accessToken/expiresAt from the
		// authorization_code exchange - no need for a second round trip.
		return c.accessToken, nil
	}

	payload := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     c.clientID,
		"client_secret": c.clientSecret,
		"refresh_token": c.refreshToken,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://signin.stampsendicia.com/oauth/token", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("stamps.com oauth failed (%d): %s", resp.StatusCode, string(body))
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
	c.accessToken = parsed.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(secs-60) * time.Second)
	return c.accessToken, nil
}

// authorizeLocked performs the OAuth 2.0 authorization_code flow
// interactively and caches the result on c: it starts a temporary local
// HTTP server at c.redirectURI (or defaultRedirectURI) to receive the
// callback, prints the login URL to stdout for the user to open in their
// own browser, waits (up to 5 minutes) for the resulting code, and
// exchanges it for an access_token + refresh_token. Callers must already
// hold c.mu - this blocks for as long as the login takes, which is exactly
// the point: any other goroutine calling token() concurrently should wait
// for the same login rather than start a second one (a second HTTP
// listener on the same port would just fail to bind anyway).
func (c *Checker) authorizeLocked(ctx context.Context) error {
	redirectURI := c.redirectURI
	if redirectURI == "" {
		redirectURI = defaultRedirectURI
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		return fmt.Errorf("invalid Stamps.com redirect URI %q: %w", redirectURI, err)
	}

	type callbackResult struct {
		code string
		err  error
	}
	resultCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	path := u.Path
	if path == "" {
		path = "/"
	}
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errParam := q.Get("error"); errParam != "" {
			desc := q.Get("error_description")
			fmt.Fprintf(w, "Stamps.com login failed: %s (%s). You can close this window.", errParam, desc)
			select {
			case resultCh <- callbackResult{err: fmt.Errorf("stamps.com authorization failed: %s: %s", errParam, desc)}:
			default:
			}
			return
		}
		code := q.Get("code")
		if code == "" {
			fmt.Fprint(w, "No authorization code received. You can close this window.")
			select {
			case resultCh <- callbackResult{err: fmt.Errorf("stamps.com authorization callback had no code")}:
			default:
			}
			return
		}
		fmt.Fprint(w, "Stamps.com login successful - you can close this window and return to the terminal.")
		select {
		case resultCh <- callbackResult{code: code}:
		default:
		}
	})

	ln, err := net.Listen("tcp", u.Host)
	if err != nil {
		return fmt.Errorf("could not start local callback server on %s (must match the redirect URI registered "+
			"for this client ID in the Stamps.com/Endicia developer console): %w", u.Host, err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	authorizeURL := "https://signin.stampsendicia.com/authorize?" + url.Values{
		"client_id":     {c.clientID},
		"response_type": {"code"},
		"redirect_uri":  {redirectURI},
		"scope":         {"offline_access"},
	}.Encode()

	fmt.Fprintln(os.Stdout, "\nStamps.com/Endicia: no cached refresh token - open this URL in a browser to log in")
	fmt.Fprintln(os.Stdout, "(waiting up to 5 minutes; the redirect URI above must be registered for this client ID):")
	fmt.Fprintln(os.Stdout, authorizeURL)

	select {
	case res := <-resultCh:
		if res.err != nil {
			return res.err
		}
		return c.exchangeCodeLocked(ctx, res.code, redirectURI)
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("timed out waiting for Stamps.com login (5 min)")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// exchangeCodeLocked trades an authorization code for an access_token and
// refresh_token and caches both on c (accessToken/expiresAt/refreshToken).
// Callers must already hold c.mu.
func (c *Checker) exchangeCodeLocked(ctx context.Context, code, redirectURI string) error {
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     c.clientID,
		"client_secret": c.clientSecret,
		"code":          code,
		"redirect_uri":  redirectURI,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://signin.stampsendicia.com/oauth/token", bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stamps.com authorization_code exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    string `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return err
	}
	if parsed.RefreshToken == "" {
		return fmt.Errorf("stamps.com did not return a refresh_token - was scope=offline_access included in the login request?")
	}
	secs, _ := strconv.Atoi(parsed.ExpiresIn)
	if secs <= 60 {
		secs = 3600
	}
	c.accessToken = parsed.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(secs-60) * time.Second)
	c.refreshToken = parsed.RefreshToken

	fmt.Fprintln(os.Stdout, "Stamps.com login complete for this run. To skip this login next time, save this")
	fmt.Fprintln(os.Stdout, "refresh token as STAMPS_REFRESH_TOKEN in your .env (treat it like a password):")
	fmt.Fprintln(os.Stdout, parsed.RefreshToken)

	return nil
}

// Ping just does the OAuth exchange and throws away the token - if that
// succeeds, the credentials are good. If no refresh token is cached yet,
// this is what triggers the interactive login (see Checker's doc comment).
func (c *Checker) Ping(ctx context.Context) error {
	_, err := c.token(ctx)
	return err
}

// Scanned reports whether trackingNumber has recorded at least one physical
// scan (not just a label). Unlike UPS/FedEx/USPS's own carrier-specific
// status codes, SERA standardizes tracking status across every carrier it
// supports into one small enum (status_code), so there's no carrier
// activity list to walk here.
func (c *Checker) Scanned(ctx context.Context, trackingNumber string) (bool, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return false, err
	}

	reqURL := fmt.Sprintf("https://api.stampsendicia.com/sera/v1/tracking?carrier=usps&tracking_number=%s",
		url.QueryEscape(trackingNumber))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("stamps.com track failed (%d): %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		StatusCode string `json:"status_code"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, err
	}

	// SERA's status_code is one of: awaiting_shipment, accepted, in_transit,
	// delivered, exception (per the SERA OpenAPI spec's /v1/tracking
	// response). "awaiting_shipment" means only an electronic label exists
	// with no physical scan yet; anything else means USPS has physically
	// scanned the package at least once. Verify against Stamps.com's
	// current SERA docs if this ever looks wrong.
	return !strings.EqualFold(parsed.StatusCode, "awaiting_shipment"), nil
}
