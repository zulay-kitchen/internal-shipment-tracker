// Package ups checks UPS's Tracking API to see whether a shipment has been
// physically scanned yet (not just manifested/labeled).
package ups

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// trackingNumberRe matches UPS's own tracking number format.
var trackingNumberRe = regexp.MustCompile(`^1Z[0-9A-Z]{16}$`)

// LooksLikeTrackingNumber reports whether trackingNumber's shape
// unambiguously matches UPS's own tracking number format. This is
// deliberately conservative: it should only match formats distinctive
// enough not to collide with another carrier's own format - see callers
// such as cmd/tracker's detectCarrierFromTrackingNumber, which uses this to
// decide whether an "amazon_shipping" label is really just a relabeled UPS
// shipment.
func LooksLikeTrackingNumber(trackingNumber string) bool {
	tn := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(trackingNumber), " ", ""))
	return trackingNumberRe.MatchString(tn)
}

// Checker calls UPS's Tracking API using OAuth client_credentials
// credentials.
type Checker struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// New returns a Checker for the given UPS OAuth client ID/secret.
func New(clientID, clientSecret string) *Checker {
	return &Checker{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (u *Checker) token(ctx context.Context) (string, error) {
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
func (u *Checker) Ping(ctx context.Context) error {
	_, err := u.token(ctx)
	return err
}

// Scanned reports whether trackingNumber has recorded at least one physical
// scan (not just a manifest/label).
func (u *Checker) Scanned(ctx context.Context, trackingNumber string) (bool, error) {
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
				// docs if this ever looks wrong - this is currently under
				// investigation: some delivered/scanned UPS shipments (Mail
				// Innovations, e.g.) are being reported as not-scanned, and
				// this narrow check (only status.type, nothing else in the
				// response) is the leading suspect. See AGENTS.md.
				if strings.ToUpper(act.Status.Type) != "M" {
					return true, nil
				}
			}
		}
	}

	return false, nil
}
