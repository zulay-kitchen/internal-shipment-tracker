// Package fedex checks FedEx's Track API to see whether a shipment has been
// physically scanned yet (not just labeled).
package fedex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// trackingNumberRe matches FedEx's own tracking number format (12 or 15
// plain digits).
var trackingNumberRe = regexp.MustCompile(`^\d{12}$|^\d{15}$`)

// LooksLikeTrackingNumber reports whether trackingNumber's shape
// unambiguously matches FedEx's own tracking number format. This is
// deliberately conservative: it should only match formats distinctive
// enough not to collide with another carrier's own format - see callers
// such as cmd/tracker's detectCarrierFromTrackingNumber, which uses this to
// decide whether an "amazon_shipping" label is really just a relabeled
// FedEx shipment.
func LooksLikeTrackingNumber(trackingNumber string) bool {
	tn := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(trackingNumber), " ", ""))
	return trackingNumberRe.MatchString(tn)
}

// Checker calls FedEx's Track API using OAuth client_credentials
// credentials.
type Checker struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// New returns a Checker for the given FedEx OAuth client ID/secret.
func New(clientID, clientSecret string) *Checker {
	return &Checker{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (f *Checker) token(ctx context.Context) (string, error) {
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
func (f *Checker) Ping(ctx context.Context) error {
	_, err := f.token(ctx)
	return err
}

// Scanned reports whether trackingNumber has recorded at least one physical
// scan (not just a label).
func (f *Checker) Scanned(ctx context.Context, trackingNumber string) (bool, error) {
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
