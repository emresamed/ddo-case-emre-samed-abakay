// Package client is a small JSON client for the API, shared by the
// simulator and the verify tool.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	Base string
	HTTP *http.Client
}

func New(base string) *Client {
	return &Client{Base: base, HTTP: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		MaxIdleConns: 500, MaxIdleConnsPerHost: 500, IdleConnTimeout: 60 * time.Second,
	}}}
}

// Resp is a decoded response.
type Resp struct {
	Status int
	Body   []byte
	Header http.Header
}

// Decode unmarshals the body into v.
func (r Resp) Decode(v any) error { return json.Unmarshal(r.Body, v) }

// ErrCode returns error.code of an error body.
func (r Resp) ErrCode() string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &e)
	return e.Error.Code
}

func (r Resp) String() string {
	b := r.Body
	if len(b) > 300 {
		b = b[:300]
	}
	return fmt.Sprintf("%d %s", r.Status, b)
}

// Do sends a request. body may be nil. idemKey sets Idempotency-Key.
func (c *Client) Do(ctx context.Context, method, path, token string, body any, idemKey string) (Resp, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Resp{}, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return Resp{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return Resp{}, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	return Resp{Status: res.StatusCode, Body: b, Header: res.Header}, err
}

// Login returns an access token.
func (c *Client) Login(ctx context.Context, email, password string) (string, error) {
	r, err := c.Do(ctx, http.MethodPost, "/api/auth/login", "", map[string]string{"email": email, "password": password}, "")
	if err != nil {
		return "", err
	}
	if r.Status != http.StatusOK {
		return "", fmt.Errorf("login %s: %s", email, r)
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	return t.AccessToken, r.Decode(&t)
}

// WaitHealthy polls /health until it answers 200 or the timeout passes.
func (c *Client) WaitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		r, err := c.Do(ctx, http.MethodGet, "/health", "", nil, "")
		if err == nil && r.Status == http.StatusOK {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("api not healthy after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Types used by the tools.
type Showtime struct {
	ID         int64     `json:"id"`
	MovieTitle string    `json:"movie_title"`
	HallID     int64     `json:"hall_id"`
	HallName   string    `json:"hall_name"`
	StartsAt   time.Time `json:"starts_at"`
	Capacity   int       `json:"capacity"`
	Taken      int       `json:"taken"`
}

type Seat struct {
	ID     int64  `json:"id"`
	Row    int    `json:"row"`
	Col    int    `json:"col"`
	Label  string `json:"label"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

type SeatMap struct {
	Showtime Showtime         `json:"showtime"`
	Seats    []Seat           `json:"seats"`
	Prices   map[string]int64 `json:"prices"`
}

type Suggestion struct {
	SeatIDs []int64 `json:"seat_ids"`
	Row     int     `json:"row"`
}

type Hold struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
	SeatIDs   []int64   `json:"seat_ids"`
	Extensions int      `json:"extensions"`
}

type OrderLine struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	TicketID string `json:"ticket_id"`
	SeatID   int64  `json:"seat_id"`
	Paid     int64  `json:"paid"`
	GiftPaid int64  `json:"gift_paid"`
	Status   string `json:"status"`
	Tax      struct {
		Net    int64 `json:"net"`
		EntTax int64 `json:"ent_tax"`
		VAT    int64 `json:"vat"`
	} `json:"tax"`
}

type Order struct {
	ID         string      `json:"id"`
	ShowtimeID int64       `json:"showtime_id"`
	Status     string      `json:"status"`
	Total      int64       `json:"total"`
	Discount   int64       `json:"discount"`
	GiftPaid   int64       `json:"gift_paid"`
	Channel    string      `json:"channel"`
	Lines      []OrderLine `json:"lines"`
}
