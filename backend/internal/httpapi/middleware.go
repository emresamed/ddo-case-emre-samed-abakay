package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"sahnebilet/internal/service"
)

var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{Name: "http_requests_total", Help: "HTTP requests."}, []string{"method", "route", "status"})
	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "http_request_duration_seconds", Help: "HTTP latency.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}}, []string{"method", "route"})
)

type ctxKey int

const (
	keyActor ctxKey = iota
	keyRequestID
)

type statusWriter struct {
	http.ResponseWriter
	status int
	size   int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.size += n
	return n, err
}

func requestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// observe adds request id, panic recovery, timeout, structured access log
// and Prometheus metrics. Query strings and bodies are never logged.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-Id")
		if rid == "" || len(rid) > 64 {
			rid = requestID()
		}
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), keyRequestID, rid), s.svc.Cfg.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		sw := &statusWriter{ResponseWriter: w}
		sw.Header().Set("X-Request-Id", rid)
		sw.Header().Set("X-Content-Type-Options", "nosniff")
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic", "request_id", rid, "panic", p, "stack", string(debug.Stack()))
				if sw.status == 0 {
					writeJSON(sw, http.StatusInternalServerError, errBody("INTERNAL", "internal error"))
				}
			}
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			httpRequests.WithLabelValues(r.Method, route, strconv.Itoa(status)).Inc()
			httpDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
			if route != "GET /metrics" && route != "GET /health" {
				attrs := []any{"request_id", rid, "method", r.Method, "path", r.URL.Path, "status", status,
					"duration_ms", time.Since(start).Milliseconds(), "bytes", sw.size}
				if a, ok := r.Context().Value(keyActor).(*service.Actor); ok && a.UserID > 0 {
					attrs = append(attrs, "user_id", a.UserID, "role", a.Role)
				}
				s.log.Info("http", attrs...)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// handler is an authenticated handler.
type handler func(w http.ResponseWriter, r *http.Request, a service.Actor)

// auth checks the bearer token and the role. 401 without a valid token,
// 403 with a valid token of a role that is not allowed.
func (s *Server) auth(h handler, roles ...string) http.Handler {
	allowed := map[string]bool{}
	for _, r := range roles {
		allowed[r] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			writeJSON(w, http.StatusUnauthorized, errBody("UNAUTHENTICATED", "missing bearer token"))
			return
		}
		a, err := s.svc.ParseAccess(tok)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, errBody("UNAUTHENTICATED", "invalid or expired token"))
			return
		}
		if holder, ok := r.Context().Value(keyActor).(*service.Actor); ok {
			*holder = a
		}
		if len(allowed) > 0 && !allowed[a.Role] {
			writeJSON(w, http.StatusForbidden, errBody("FORBIDDEN", "role "+a.Role+" cannot access this endpoint"))
			return
		}
		h(w, r, a)
	})
}

// withActorSlot lets the access log see the user resolved by auth.
func withActorSlot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyActor, &service.Actor{})))
	})
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

// idem implements the Idempotency-Key header for state changing POSTs.
// The first request with a key runs and its response is stored; a retry
// with the same key and body gets the stored response; the same key with a
// different body is rejected; a retry while the first is running gets 409.
// 5xx responses are not stored so that the client can retry.
func (s *Server) idem(h handler) handler {
	return func(w http.ResponseWriter, r *http.Request, a service.Actor) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > 100 {
			writeJSON(w, http.StatusBadRequest, errBody("IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header (1-100 chars) is required"))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(body) > maxBody {
			writeJSON(w, http.StatusRequestEntityTooLarge, errBody("BODY_TOO_LARGE", "request body too large"))
			return
		}
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		ctx := r.Context()
		tag, err := s.svc.DB.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, request_hash) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
			a.UserID, key, sum[:])
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if tag.RowsAffected() == 0 {
			var hash, resp []byte
			var status *int
			err := s.svc.DB.QueryRow(ctx, `SELECT request_hash, status_code, response FROM idempotency_keys WHERE user_id = $1 AND key = $2`, a.UserID, key).
				Scan(&hash, &status, &resp)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				writeJSON(w, http.StatusConflict, errBody("IDEMPOTENCY_IN_PROGRESS", "retry"))
			case err != nil:
				s.fail(w, r, err)
			case !bytes.Equal(hash, sum[:]):
				writeJSON(w, http.StatusUnprocessableEntity, errBody("IDEMPOTENCY_KEY_REUSED", "key was used with a different request"))
			case status == nil:
				writeJSON(w, http.StatusConflict, errBody("IDEMPOTENCY_IN_PROGRESS", "the original request is still running"))
			default:
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(*status)
				_, _ = w.Write(resp)
			}
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &recorder{header: http.Header{}}
		h(rec, r, a)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		// store with a fresh context: the request context may be cancelled
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if rec.status >= 500 {
			_, err = s.svc.DB.Exec(bg, `DELETE FROM idempotency_keys WHERE user_id = $1 AND key = $2`, a.UserID, key)
		} else {
			_, err = s.svc.DB.Exec(bg, `UPDATE idempotency_keys SET status_code = $3, response = $4 WHERE user_id = $1 AND key = $2`,
				a.UserID, key, rec.status, rec.body.Bytes())
		}
		if err != nil {
			s.log.Error("idempotency store failed", "error", err)
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	}
}

const maxBody = 1 << 20

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &service.Error{Status: http.StatusBadRequest, Code: "BAD_JSON", Message: "invalid JSON body: " + err.Error()}
	}
	return nil
}

func errBody(code, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": msg}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail maps errors to the uniform error body.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var e *service.Error
	if errors.As(err, &e) {
		writeJSON(w, e.Status, map[string]any{"error": e})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeJSON(w, http.StatusServiceUnavailable, errBody("TIMEOUT", "request timed out"))
		return
	}
	rid, _ := r.Context().Value(keyRequestID).(string)
	s.log.Error("request failed", "request_id", rid, "path", r.URL.Path, "error", err.Error())
	writeJSON(w, http.StatusInternalServerError, errBody("INTERNAL", "internal error, request id "+rid))
}
