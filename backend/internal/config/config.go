// Package config reads all settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr             string
	DatabaseURL      string
	JWTSecret        []byte
	AccessTTL        time.Duration
	RefreshTTL       time.Duration
	HoldTTL          time.Duration
	TicketSeed       string // base64 32-byte Ed25519 seed
	TimeZone         *time.Location
	RequestTimeout   time.Duration
	SeedValue        int64
	ScheduleDays     int
	BcryptCost       int
	AllowedOrigin    string
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func envDur(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}

// Load reads and validates the configuration.
func Load() (Config, error) {
	var c Config
	var err error
	c.Addr = env("HTTP_ADDR", ":8080")
	c.DatabaseURL = os.Getenv("DATABASE_URL")
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	secret := os.Getenv("JWT_SECRET")
	if len(secret) < 32 {
		return c, fmt.Errorf("JWT_SECRET must be at least 32 characters")
	}
	c.JWTSecret = []byte(secret)
	c.TicketSeed = os.Getenv("TICKET_SIGNING_SEED")
	if c.TicketSeed == "" {
		return c, fmt.Errorf("TICKET_SIGNING_SEED is required")
	}
	if c.AccessTTL, err = envDur("ACCESS_TOKEN_TTL", 15*time.Minute); err != nil {
		return c, err
	}
	if c.RefreshTTL, err = envDur("REFRESH_TOKEN_TTL", 7*24*time.Hour); err != nil {
		return c, err
	}
	if c.HoldTTL, err = envDur("HOLD_TTL", 10*time.Minute); err != nil {
		return c, err
	}
	if c.RequestTimeout, err = envDur("REQUEST_TIMEOUT", 10*time.Second); err != nil {
		return c, err
	}
	if c.TimeZone, err = time.LoadLocation(env("TZ_NAME", "Europe/Istanbul")); err != nil {
		return c, err
	}
	seed, err := envInt("SEED", 42)
	if err != nil {
		return c, err
	}
	c.SeedValue = int64(seed)
	if c.ScheduleDays, err = envInt("SCHEDULE_DAYS", 14); err != nil {
		return c, err
	}
	if c.BcryptCost, err = envInt("BCRYPT_COST", 10); err != nil {
		return c, err
	}
	c.AllowedOrigin = env("ALLOWED_ORIGIN", "http://localhost:3000")
	return c, nil
}
