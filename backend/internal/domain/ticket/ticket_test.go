package ticket

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func key(t *testing.T, b byte) ed25519.PrivateKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = b
	}
	k, err := KeyFromSeed(base64.StdEncoding.EncodeToString(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var now = time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)

func claims() Claims {
	return Claims{TicketID: "abc", ShowtimeID: 7, Seat: "F-8", NotBefore: now.Add(-time.Hour).Unix(), NotAfter: now.Add(time.Hour).Unix()}
}

func TestSignVerify_RoundTrip(t *testing.T) {
	k := key(t, 1)
	code, err := Sign(k, claims())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, Prefix+".") {
		t.Fatalf("bad prefix %s", code)
	}
	c, err := Verify(k.Public().(ed25519.PublicKey), code, now)
	if err != nil || c != claims() {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestVerify_WrongKeyAndTamper(t *testing.T) {
	k := key(t, 1)
	code, _ := Sign(k, claims())
	if _, err := Verify(key(t, 2).Public().(ed25519.PublicKey), code, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("want ErrSignature, got %v", err)
	}
	other := claims()
	other.Seat = "A-1"
	forged, _ := Sign(k, other)
	parts := strings.Split(code, ".")
	fparts := strings.Split(forged, ".")
	tampered := parts[0] + "." + fparts[1] + "." + parts[2]
	if _, err := Verify(k.Public().(ed25519.PublicKey), tampered, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("payload swap must fail, got %v", err)
	}
}

func TestVerify_TimeWindowAndMalformed(t *testing.T) {
	k := key(t, 1)
	pub := k.Public().(ed25519.PublicKey)
	code, _ := Sign(k, claims())
	if _, err := Verify(pub, code, now.Add(-2*time.Hour)); !errors.Is(err, ErrNotYet) {
		t.Fatalf("want ErrNotYet, got %v", err)
	}
	if _, err := Verify(pub, code, now.Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	for _, bad := range []string{"", "SB1.x", "XX.a.b", "SB1.!!.??"} {
		if _, err := Verify(pub, bad, now); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%q: want ErrMalformed, got %v", bad, err)
		}
	}
}

func TestKeyFromSeed_Invalid(t *testing.T) {
	if _, err := KeyFromSeed("short"); err == nil {
		t.Fatal("expected error")
	}
}
