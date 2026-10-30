package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Tokens is the login response.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	User         Actor  `json:"user"`
}

var errBadCredentials = E(http.StatusUnauthorized, "INVALID_CREDENTIALS", "email or password is wrong")

// dummyHash keeps login timing equal for unknown e-mails.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equaliser"), bcrypt.MinCost)

type accessClaims struct {
	Role  string `json:"role"`
	Email string `json:"email"`
	jwt.RegisteredClaims
}

// HashPassword hashes with bcrypt using the configured cost.
func (s *Service) HashPassword(p string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(p), s.Cfg.BcryptCost)
	return string(h), err
}

// Login checks credentials and issues an access + refresh token pair.
func (s *Service) Login(ctx context.Context, email, password string) (Tokens, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" || password == "" {
		return Tokens{}, invalid("email and password are required")
	}
	var a Actor
	var hash string
	err := s.DB.QueryRow(ctx, `SELECT id, role, email, password_hash FROM users WHERE lower(email) = $1`, email).
		Scan(&a.UserID, &a.Role, &a.Email, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return Tokens{}, errBadCredentials
	}
	if err != nil {
		return Tokens{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return Tokens{}, errBadCredentials
	}
	return s.issue(ctx, a)
}

func (s *Service) issue(ctx context.Context, a Actor) (Tokens, error) {
	now := s.Now()
	at, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{
		Role: a.Role, Email: a.Email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(a.UserID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.Cfg.AccessTTL)),
			Issuer:    "sahnebilet",
		},
	}).SignedString(s.Cfg.JWTSecret)
	if err != nil {
		return Tokens{}, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Tokens{}, err
	}
	rt := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(rt))
	if _, err := s.DB.Exec(ctx, `INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1,$2,$3)`,
		a.UserID, sum[:], now.Add(s.Cfg.RefreshTTL)); err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: at, RefreshToken: rt, ExpiresIn: int(s.Cfg.AccessTTL.Seconds()), User: a}, nil
}

// Refresh rotates a refresh token: the old one is revoked atomically, so a
// stolen token can be used at most once.
func (s *Service) Refresh(ctx context.Context, token string) (Tokens, error) {
	sum := sha256.Sum256([]byte(token))
	var a Actor
	err := s.DB.QueryRow(ctx, `
		UPDATE refresh_tokens rt SET revoked_at = now()
		FROM users u
		WHERE rt.token_hash = $1 AND rt.revoked_at IS NULL AND rt.expires_at > now() AND u.id = rt.user_id
		RETURNING u.id, u.role, u.email`, sum[:]).Scan(&a.UserID, &a.Role, &a.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tokens{}, E(http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "refresh token is invalid or expired")
	}
	if err != nil {
		return Tokens{}, err
	}
	return s.issue(ctx, a)
}

// Logout revokes a refresh token.
func (s *Service) Logout(ctx context.Context, token string) error {
	sum := sha256.Sum256([]byte(token))
	_, err := s.DB.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, sum[:])
	return err
}

// ParseAccess validates an access token.
func (s *Service) ParseAccess(token string) (Actor, error) {
	var c accessClaims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) { return s.Cfg.JWTSecret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("sahnebilet"), jwt.WithTimeFunc(s.Now), jwt.WithLeeway(5*time.Second))
	if err != nil {
		return Actor{}, err
	}
	id, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil {
		return Actor{}, err
	}
	return Actor{UserID: id, Role: c.Role, Email: c.Email}, nil
}
