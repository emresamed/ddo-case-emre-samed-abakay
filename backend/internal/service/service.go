// Package service holds the use cases. It owns transactions and SQL and
// delegates pure business rules to the domain packages.
package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"sahnebilet/internal/config"
	"sahnebilet/internal/domain/ticket"
)

// Roles.
const (
	RoleCustomer    = "customer"
	RoleBoxOffice   = "box_office"
	RoleHallManager = "hall_manager"
	RoleProgrammer  = "programmer"
	RoleAccounting  = "accounting"
	RoleGate        = "gate"
)

// Actor is the authenticated caller.
type Actor struct {
	UserID int64  `json:"user_id"`
	Role   string `json:"role"`
	Email  string `json:"email,omitempty"`
}

// Service is the application layer.
type Service struct {
	DB      *pgxpool.Pool
	Cfg     config.Config
	signKey ed25519.PrivateKey
	Now     func() time.Time
}

// New builds the service.
func New(pool *pgxpool.Pool, cfg config.Config) (*Service, error) {
	k, err := ticket.KeyFromSeed(cfg.TicketSeed)
	if err != nil {
		return nil, err
	}
	return &Service{DB: pool, Cfg: cfg, signKey: k, Now: time.Now}, nil
}

// PublicKey is the ticket verification key handed to gate scanners.
func (s *Service) PublicKey() ed25519.PublicKey { return s.signKey.Public().(ed25519.PublicKey) }

// Error is a domain error with an HTTP mapping.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// E creates an Error.
func E(status int, code, msg string) *Error { return &Error{Status: status, Code: code, Message: msg} }

var (
	ErrNotFound  = E(http.StatusNotFound, "NOT_FOUND", "resource not found")
	ErrForbidden = E(http.StatusForbidden, "FORBIDDEN", "not allowed for this role")
)

func invalid(msg string) *Error { return E(http.StatusUnprocessableEntity, "VALIDATION_ERROR", msg) }

// isLockTimeout reports lock_timeout / deadlock / serialization failures.
func isRetryable(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "55P03" || pg.Code == "40P01" || pg.Code == "40001"
	}
	return false
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// inTx runs fn in a transaction with a lock timeout so that a hot row never
// blocks a request for longer than the HTTP timeout.
func (s *Service) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '3s'"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		if isRetryable(err) {
			return E(http.StatusConflict, "BUSY", "seats are being processed by other customers, retry")
		}
		return err
	}
	return tx.Commit(ctx)
}

// audit appends to the append-only audit log inside the caller's tx.
func audit(ctx context.Context, tx pgx.Tx, actor int64, action, entity, id string, oldV, newV any) error {
	enc := func(v any) ([]byte, error) {
		if v == nil {
			return nil, nil
		}
		return json.Marshal(v)
	}
	o, err := enc(oldV)
	if err != nil {
		return err
	}
	n, err := enc(newV)
	if err != nil {
		return err
	}
	var actorArg any
	if actor > 0 {
		actorArg = actor
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (actor_id, action, entity, entity_id, old_value, new_value)
		VALUES ($1,$2,$3,$4,$5,$6)`, actorArg, action, entity, id, o, n)
	return err
}

// newID returns a random RFC 4122 v4 UUID.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func hasRole(a Actor, roles ...string) bool {
	for _, r := range roles {
		if a.Role == r {
			return true
		}
	}
	return false
}

func b64std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
