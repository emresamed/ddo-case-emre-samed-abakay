// Package ticket signs and verifies ticket QR payloads with Ed25519. Gate
// scanners only need the public key, so they can verify tickets offline.
package ticket

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Prefix versions the QR format.
const Prefix = "SB1"

var b64 = base64.RawURLEncoding

var (
	ErrMalformed = errors.New("malformed ticket code")
	ErrSignature = errors.New("invalid ticket signature")
	ErrNotYet    = errors.New("ticket not valid yet")
	ErrExpired   = errors.New("ticket expired")
)

// Claims is the signed content of a ticket QR.
type Claims struct {
	TicketID   string `json:"t"`
	ShowtimeID int64  `json:"s"`
	Seat       string `json:"l"`
	NotBefore  int64  `json:"nb"` // unix seconds; doors open
	NotAfter   int64  `json:"na"` // unix seconds; show end
}

// Sign returns "SB1.<payload>.<signature>", both base64url without padding.
func Sign(priv ed25519.PrivateKey, c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	p := b64.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(Prefix+"."+p))
	return Prefix + "." + p + "." + b64.EncodeToString(sig), nil
}

// Parse verifies the signature and decodes the claims without checking time.
func Parse(pub ed25519.PublicKey, code string) (Claims, error) {
	parts := strings.Split(strings.TrimSpace(code), ".")
	if len(parts) != 3 || parts[0] != Prefix {
		return Claims{}, ErrMalformed
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Claims{}, ErrMalformed
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, ErrSignature
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil || c.TicketID == "" {
		return Claims{}, ErrMalformed
	}
	return c, nil
}

// Verify parses the code and checks its validity window against now.
func Verify(pub ed25519.PublicKey, code string, now time.Time) (Claims, error) {
	c, err := Parse(pub, code)
	if err != nil {
		return c, err
	}
	if now.Unix() < c.NotBefore {
		return c, ErrNotYet
	}
	if now.Unix() > c.NotAfter {
		return c, ErrExpired
	}
	return c, nil
}

// KeyFromSeed derives the signing key from a 32-byte seed given as base64.
func KeyFromSeed(seedB64 string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("TICKET_SIGNING_SEED must be 32 bytes, base64 encoded")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
