// Package goflow is a minimal Goflow API client scoped to what this
// program needs: pulling orders marked "shipped" in a date range. It
// transparently retries on 429s using the Retry-After header Goflow
// returns.
package goflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Weight is a box's weight, as reported by Goflow (e.g. {"amount": 256,
// "measure": "ounces"}). nil on a Box when Goflow hasn't recorded one.
type Weight struct {
	Amount  float64 `json:"amount"`
	Measure string  `json:"measure"`
}

// Dimensions is a box's physical size, as reported by Goflow (e.g.
// {"length": 11, "width": 8, "height": 17, "measure": "inches"}). nil on a
// Box when Goflow hasn't recorded one.
type Dimensions struct {
	Length  float64 `json:"length"`
	Width   float64 `json:"width"`
	Height  float64 `json:"height"`
	Measure string  `json:"measure"`
}

// Cost is a monetary amount attached to a Box (its shipping charge).
// Goflow nests a currency object alongside amount; only amount is modeled
// here since nothing in this module needs the currency yet.
type Cost struct {
	Amount float64 `json:"amount"`
}

// Box is a single tracked package within a Shipment.
type Box struct {
	TrackingNumber *string     `json:"tracking_number"`
	Weight         *Weight     `json:"weight"`
	Dimensions     *Dimensions `json:"dimensions"`
	Cost           *Cost       `json:"cost"`
}

// Shipment is the shipping details attached to an Order.
type Shipment struct {
	Carrier        *string    `json:"carrier"`
	ShippingMethod *string    `json:"shipping_method"`
	ShippedAt      *time.Time `json:"shipped_at"`
	Boxes          []Box      `json:"boxes"`
}

// ShippingAddress is an Order's destination address. Only the fields this
// module currently needs are modeled; Goflow's address object has more
// (name, street, city, country, contact info, ...).
type ShippingAddress struct {
	ZIPCode string `json:"zip_code"`
	State   string `json:"state"`
}

// Order is a single Goflow order, as returned by the Orders API.
type Order struct {
	ID              int64            `json:"id"`
	OrderNumber     string           `json:"order_number"`
	ShippingAddress *ShippingAddress `json:"shipping_address"`
	Shipment        *Shipment        `json:"shipment"`
}

type ordersPage struct {
	Data []Order `json:"data"`
	Next *string `json:"next"`
}

// Client is a Goflow API client for a single subdomain/token pair.
type Client struct {
	Subdomain  string
	Token      string
	HTTPClient *http.Client

	// Notice, if non-nil, gets one line of text for every notable thing
	// this client does: a request being rate-limited (429) and about to
	// be retried, and each successful pull of a page of orders (how many
	// orders came back, and whether another page will follow). Wire it to
	// your own console/log fan-out if you want that visible. If nil, all
	// of the above just happens silently.
	Notice io.Writer
}

// NewClient returns a Client ready to use, with a 30s HTTP timeout (Goflow
// order pages can be large, and pagination means several requests in a
// row).
func NewClient(subdomain, token string) *Client {
	return &Client{
		Subdomain:  subdomain,
		Token:      token,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) notef(format string, args ...interface{}) {
	if c.Notice == nil {
		return
	}
	fmt.Fprintf(c.Notice, format, args...)
}

// buildRawQuery joins key=value pairs into a query string without
// URL-encoding the keys. Goflow's filter syntax relies on literal square
// brackets and colons in keys like "filters[status_updated_at:gte]" - most
// servers tolerate the percent-encoded form (net/url's default), but this
// keeps the request exactly as documented. Values are still escaped
// normally.
func buildRawQuery(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p[0]+"="+url.QueryEscape(p[1]))
	}
	return strings.Join(parts, "&")
}

// retryAfterDelay parses a Retry-After header value (Goflow documents this
// as the number of seconds to wait before retrying a rate-limited request)
// and rounds it up to the nearest whole second. If the header is missing or
// unparseable, it falls back to a conservative 1 second delay.
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

// doRequest performs a Goflow API request, transparently waiting and
// retrying if Goflow responds with a 429 (rate limited), per the
// Retry-After header it returns.
func (c *Client) doRequest(ctx context.Context, method, reqURL string) ([]byte, int, error) {
	for {
		req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/json")

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, 0, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, 0, readErr
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			delay := retryAfterDelay(resp.Header.Get("Retry-After"))
			c.notef("Goflow rate limit hit; waiting %d second(s) before retrying...\n", int(delay.Seconds()))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
			continue
		}

		return body, resp.StatusCode, nil
	}
}

// FetchShippedOrders pulls every order with status "shipped" whose
// status_updated_at falls within [start, endExclusive). Goflow doesn't
// support filtering directly on shipment.shipped_at, so this is used as a
// close approximation; callers should still check shipment.shipped_at
// themselves against the exact window they care about.
func (c *Client) FetchShippedOrders(ctx context.Context, start, endExclusive time.Time) ([]Order, error) {
	query := buildRawQuery([][2]string{
		{"filters[status]", "shipped"},
		{"filters[status_updated_at:gte]", start.UTC().Format(time.RFC3339)},
		{"filters[status_updated_at:lt]", endExclusive.UTC().Format(time.RFC3339)},
		{"sort", "status_updated_at"},
		{"sort_direction", "asc"},
	})

	next := fmt.Sprintf("https://%s.api.goflow.com/v1/orders?%s", c.Subdomain, query)

	var all []Order
	for next != "" {
		body, statusCode, err := c.doRequest(ctx, http.MethodGet, next)
		if err != nil {
			return nil, err
		}
		if statusCode != http.StatusOK {
			return nil, fmt.Errorf("goflow API returned %d: %s", statusCode, string(body))
		}

		var page ordersPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		c.notef("Goflow contacted and %d orders pulled.\n", len(page.Data))

		if page.Next != nil && *page.Next != "" {
			next = *page.Next
			c.notef("More Goflow orders indicated.\n")
		} else {
			next = ""
		}
	}
	return all, nil
}
