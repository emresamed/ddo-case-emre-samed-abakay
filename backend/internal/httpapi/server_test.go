package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"sahnebilet/internal/config"
	"sahnebilet/internal/service"
)

// These tests cover the HTTP layer without a database: everything that is
// decided before a handler reaches the service (auth, RBAC, input checks,
// error mapping, headers). Database-backed behaviour is covered by the
// service integration tests and by `make verify`.

const secret = "test-secret-test-secret-test-secret!"

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	cfg := config.Config{
		JWTSecret: []byte(secret), AccessTTL: time.Minute, RequestTimeout: 5 * time.Second,
		TicketSeed: "c2FobmViaWxldC1kZW1vLXRpY2tldC1zZWVkLTMyYiE=", TimeZone: time.UTC, HoldTTL: 10 * time.Minute,
	}
	svc, err := service.New(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Server{svc: svc, log: log}, New(svc, log, make(chan struct{}))
}

func token(t *testing.T, role string, exp time.Duration) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "7", "role": role, "email": role + "@test", "iss": "sahnebilet",
		"iat": time.Now().Unix(), "exp": time.Now().Add(exp).Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func call(h http.Handler, method, path, tok, body string, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("body is not the uniform error JSON: %q", rec.Body.String())
	}
	if b.Error.Message == "" {
		t.Fatalf("error without message: %q", rec.Body.String())
	}
	return b.Error.Code
}

func TestAuth_MissingInvalidAndExpiredToken(t *testing.T) {
	_, h := newTestServer(t)
	for name, tok := range map[string]string{
		"missing": "",
		"garbage": "not-a-jwt",
		"expired": token(t, "customer", -time.Hour),
		"wrong signature": func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "1", "role": "customer", "iss": "sahnebilet",
				"exp": time.Now().Add(time.Hour).Unix()}).SignedString([]byte("another-secret-another-secret-12"))
			return s
		}(),
	} {
		rec := call(h, "GET", "/api/me", tok, "")
		if rec.Code != 401 || errCode(t, rec) != "UNAUTHENTICATED" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestAuth_RejectsNoneAlgorithm(t *testing.T) {
	_, h := newTestServer(t)
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "1", "role": "accounting", "iss": "sahnebilet",
		"exp": time.Now().Add(time.Hour).Unix()}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if rec := call(h, "GET", "/api/me", tok, ""); rec.Code != 401 {
		t.Fatalf("alg=none accepted: %d", rec.Code)
	}
}

func TestAuth_ValidTokenReturnsActor(t *testing.T) {
	_, h := newTestServer(t)
	rec := call(h, "GET", "/api/me", token(t, "box_office", time.Minute), "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var a service.Actor
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	if a.UserID != 7 || a.Role != "box_office" {
		t.Fatalf("%+v", a)
	}
}

// The RBAC matrix: every one of these must be refused with 403 before any
// handler code runs.
func TestRBAC_ForbiddenRoutes(t *testing.T) {
	_, h := newTestServer(t)
	cases := []struct{ role, method, path string }{
		{"customer", "GET", "/api/reports/revenue?from=2026-01-01&to=2026-01-02"},
		{"customer", "GET", "/api/reports/distributor?from=2026-01-01&to=2026-01-02"},
		{"customer", "GET", "/api/reports/ab?from=2026-01-01&to=2026-01-02"},
		{"customer", "POST", "/api/gate/scan"},
		{"customer", "POST", "/api/gate/sync"},
		{"customer", "PATCH", "/api/seats/1"},
		{"customer", "POST", "/api/movies"},
		{"customer", "GET", "/api/audit"},
		{"customer", "POST", "/api/gift-cards"},
		{"customer", "GET", "/api/showtimes/1/integrity"},
		{"gate", "POST", "/api/holds"},
		{"gate", "POST", "/api/orders"},
		{"gate", "GET", "/api/tickets/x/qr"},
		{"gate", "GET", "/api/offers/me"},
		{"box_office", "POST", "/api/schedule/generate"},
		{"box_office", "POST", "/api/coupons"},
		{"accounting", "POST", "/api/gate/sync"},
		{"accounting", "POST", "/api/holds"},
		{"programmer", "GET", "/api/audit"},
		{"programmer", "POST", "/api/orders"},
		{"hall_manager", "POST", "/api/movies"},
		{"hall_manager", "GET", "/api/reports/revenue?from=2026-01-01&to=2026-01-02"},
	}
	for _, c := range cases {
		rec := call(h, c.method, c.path, token(t, c.role, time.Minute), "{}", "Idempotency-Key", "k")
		if rec.Code != 403 || errCode(t, rec) != "FORBIDDEN" {
			t.Errorf("%s %s %s: want 403, got %d %s", c.role, c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestOffer_IsStableForTheSameUser(t *testing.T) {
	_, h := newTestServer(t)
	tok := token(t, "customer", time.Minute)
	a := call(h, "GET", "/api/offers/me", tok, "")
	b := call(h, "GET", "/api/offers/me", tok, "")
	if a.Code != 200 || a.Body.String() != b.Body.String() || !strings.Contains(a.Body.String(), "coupon_code") {
		t.Fatalf("%d %s / %s", a.Code, a.Body.String(), b.Body.String())
	}
}

func TestInputValidation_BeforeTheDatabase(t *testing.T) {
	_, h := newTestServer(t)
	cust := token(t, "customer", time.Minute)
	acct := token(t, "accounting", time.Minute)
	prog := token(t, "programmer", time.Minute)
	cases := []struct {
		name, method, path, tok, body string
		hdr                           []string
		status                        int
		code                          string
	}{
		{"non-numeric id", "GET", "/api/showtimes/abc/seats", cust, "", nil, 400, "BAD_ID"},
		{"negative id", "GET", "/api/showtimes/-4", cust, "", nil, 400, "BAD_ID"},
		{"hold without idempotency key", "POST", "/api/holds", cust, `{"showtime_id":1,"seat_ids":[1]}`, nil, 400, "IDEMPOTENCY_KEY_REQUIRED"},
		{"order with a 101-char key", "POST", "/api/orders", cust, `{}`, []string{"Idempotency-Key", strings.Repeat("k", 101)}, 400, "IDEMPOTENCY_KEY_REQUIRED"},
		{"broken JSON", "POST", "/api/auth/login", "", `{"email":`, nil, 400, "BAD_JSON"},
		{"unknown JSON field", "POST", "/api/auth/login", "", `{"email":"a","password":"b","admin":true}`, nil, 400, "BAD_JSON"},
		{"empty login", "POST", "/api/auth/login", "", `{"email":"","password":""}`, nil, 422, "VALIDATION_ERROR"},
		{"report without dates", "GET", "/api/reports/revenue", acct, "", nil, 400, "BAD_QUERY"},
		{"report with a bad date", "GET", "/api/reports/revenue?from=yesterday&to=2026-01-01", acct, "", nil, 400, "BAD_QUERY"},
		{"showtimes with a bad from", "GET", "/api/showtimes?from=monday", cust, "", nil, 400, "BAD_QUERY"},
		{"schedule with a bad date", "POST", "/api/schedule/generate", prog, `{"from":"14/11","days":7}`, nil, 422, "VALIDATION_ERROR"},
		{"schedule too long", "POST", "/api/schedule/generate", prog, `{"from":"2026-11-02","days":60}`, nil, 422, "VALIDATION_ERROR"},
	}
	for _, c := range cases {
		rec := call(h, c.method, c.path, c.tok, c.body, c.hdr...)
		if rec.Code != c.status || errCode(t, rec) != c.code {
			t.Errorf("%s: want %d %s, got %d %s", c.name, c.status, c.code, rec.Code, rec.Body.String())
		}
	}
}

func TestUnknownRoute_UniformError(t *testing.T) {
	_, h := newTestServer(t)
	rec := call(h, "GET", "/api/does-not-exist", "", "")
	if rec.Code != 404 || errCode(t, rec) != "NOT_FOUND" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
}

func TestHeaders_RequestIDAndNosniff(t *testing.T) {
	_, h := newTestServer(t)
	rec := call(h, "GET", "/api/me", "", "")
	if len(rec.Header().Get("X-Request-Id")) != 16 || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", rec.Header())
	}
	rec = call(h, "GET", "/api/me", "", "", "X-Request-Id", "from-the-proxy")
	if rec.Header().Get("X-Request-Id") != "from-the-proxy" {
		t.Fatal("incoming request id must be kept for log correlation")
	}
	rec = call(h, "GET", "/api/me", "", "", "X-Request-Id", strings.Repeat("x", 200))
	if rec.Header().Get("X-Request-Id") == strings.Repeat("x", 200) {
		t.Fatal("oversized request id must be replaced")
	}
}

func TestPublicEndpoints_OpenAPIAndMetrics(t *testing.T) {
	_, h := newTestServer(t)
	rec := call(h, "GET", "/api/openapi.yaml", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/api/holds") || !strings.Contains(rec.Body.String(), "/api/showtimes/{id}/events") {
		t.Fatalf("openapi %d", rec.Code)
	}
	call(h, "GET", "/api/me", "", "") // produce at least one observation
	rec = call(h, "GET", "/metrics", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `http_requests_total{method="GET",route="GET /api/me",status="401"}`) {
		t.Fatalf("metrics do not label by route pattern: %d", rec.Code)
	}
}

func TestFail_MapsErrors(t *testing.T) {
	s, _ := newTestServer(t)
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{service.E(409, "SEAT_TAKEN", "taken"), 409, "SEAT_TAKEN"},
		{service.ErrNotFound, 404, "NOT_FOUND"},
		{context.DeadlineExceeded, 503, "TIMEOUT"},
		{errors.New("connection reset by peer"), 500, "INTERNAL"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/x", nil)
		s.fail(rec, req, c.err)
		if rec.Code != c.status || errCode(t, rec) != c.code {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body.String())
		}
		if c.code == "INTERNAL" && strings.Contains(rec.Body.String(), "connection reset") {
			t.Error("internal error details leaked to the client")
		}
	}
	// the client hung up: not a server error, nothing is written for it to read
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	s.fail(rec, httptest.NewRequest("GET", "/x", nil).WithContext(ctx), context.Canceled)
	if rec.Code != 499 || rec.Body.Len() != 0 {
		t.Fatalf("cancelled request: %d %q", rec.Code, rec.Body.String())
	}
}

func TestPanicIsRecoveredAsJSON500(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.observe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }))
	rec := call(h, "GET", "/x", "", "")
	if rec.Code != 500 || errCode(t, rec) != "INTERNAL" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestStatusWriter_SupportsFlushForSSE(t *testing.T) {
	sw := &statusWriter{ResponseWriter: httptest.NewRecorder()}
	if err := http.NewResponseController(sw).Flush(); err != nil {
		t.Fatalf("SSE needs Flush through the logging wrapper: %v", err)
	}
}
