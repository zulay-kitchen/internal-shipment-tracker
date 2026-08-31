// Package usps checks USPS's Tracking API to see whether a shipment has
// been physically scanned yet (not just labeled).
package usps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// trackingNumberRe matches USPS's own tracking number formats.
var trackingNumberRe = regexp.MustCompile(`^\d{20}$|^\d{22}$|^[A-Z]{2}\d{9}US$`)

// LooksLikeTrackingNumber reports whether trackingNumber's shape
// unambiguously matches USPS's own tracking number format. This is
// deliberately conservative: it should only match formats distinctive
// enough not to collide with another carrier's own format - see callers
// such as cmd/tracker's detectCarrierFromTrackingNumber, which uses this to
// decide whether an "amazon_shipping" label is really just a relabeled
// USPS shipment.
func LooksLikeTrackingNumber(trackingNumber string) bool {
	tn := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(trackingNumber), " ", ""))
	return trackingNumberRe.MatchString(tn)
}

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

// Checker calls USPS's Tracking API using OAuth client_credentials
// credentials.
type Checker struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// New returns a Checker for the given USPS OAuth client ID/secret.
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
func (u *Checker) Ping(ctx context.Context) error {
	_, err := u.token(ctx)
	return err
}

// Scanned uses USPS's Tracking v3.2 (v3r2) API: a single POST /tracking
// call whose body and response are both JSON arrays (USPS supports batching
// up to 35 tracking numbers per call - this always sends just one). This
// replaced the older v3 API's "GET /tracking/{trackingNumber}" shape
// entirely; see the v3r2 OpenAPI spec for the current schema.
func (u *Checker) Scanned(ctx context.Context, trackingNumber string) (bool, error) {
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
