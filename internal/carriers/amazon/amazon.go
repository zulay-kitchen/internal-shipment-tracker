// Package amazon checks Amazon's Selling Partner API (SP-API) Shipping
// service to see whether an "amazon_shipping" (Buy Shipping) label has been
// physically scanned yet - distinct from "amazon_logistics", Amazon's own
// last-mile delivery network, which isn't handled here.
package amazon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Checker calls Amazon's SP-API Shipping tracking endpoint. Unlike
// UPS/FedEx/USPS, SP-API access tokens come from a refresh_token grant
// (Login With Amazon), not client_credentials, because access has to be
// tied to a specific seller's authorization - hence the extra refreshToken
// requirement.
type Checker struct {
	clientID     string
	clientSecret string
	refreshToken string
	endpoint     string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// New returns a Checker for the given SP-API LWA client ID/secret/refresh
// token and regional SP-API endpoint (e.g.
// "https://sellingpartnerapi-na.amazon.com").
func New(clientID, clientSecret, refreshToken, endpoint string) *Checker {
	return &Checker{
		clientID:     clientID,
		clientSecret: clientSecret,
		refreshToken: refreshToken,
		endpoint:     endpoint,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (a *Checker) token(ctx context.Context) (string, error) {
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
func (a *Checker) Ping(ctx context.Context) error {
	_, err := a.token(ctx)
	return err
}

// Scanned reports whether trackingNumber has recorded at least one physical
// scan.
func (a *Checker) Scanned(ctx context.Context, trackingNumber string) (bool, error) {
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
