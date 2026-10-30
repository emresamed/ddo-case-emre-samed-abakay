// Package gate implements the offline-capable entry scanner and the server
// side reconciliation of scans uploaded after a network outage (task T-5).
package gate

import (
	"crypto/ed25519"
	"errors"
	"sort"
	"time"

	"sahnebilet/internal/domain/ticket"
)

// Scan results.
const (
	Accepted        = "ACCEPTED"
	RejectSignature = "REJECTED_SIGNATURE"
	RejectMalformed = "REJECTED_MALFORMED"
	RejectNotYet    = "REJECTED_NOT_YET"
	RejectExpired   = "REJECTED_EXPIRED"
	RejectUsed      = "REJECTED_ALREADY_USED"
	RejectRefunded  = "REJECTED_REFUNDED"
	RejectWrongShow = "REJECTED_WRONG_SHOWTIME"
)

// Event is one accepted scan, recorded locally and uploaded on sync.
type Event struct {
	TicketID  string    `json:"ticket_id"`
	GateID    string    `json:"gate_id"`
	ScannedAt time.Time `json:"scanned_at"`
}

// Scanner is the state of one gate device. It works without a network: it
// verifies signatures with the public key, keeps a local used list and a
// refunded (revoked) list received at the last sync.
type Scanner struct {
	GateID    string
	pub       ed25519.PublicKey
	showtimes map[int64]bool // empty = any showtime
	used      map[string]bool
	revoked   map[string]bool
	pending   []Event
}

// NewScanner creates a scanner for the given showtimes (none = all).
func NewScanner(gateID string, pub ed25519.PublicKey, showtimes ...int64) *Scanner {
	s := &Scanner{GateID: gateID, pub: pub, showtimes: map[int64]bool{}, used: map[string]bool{}, revoked: map[string]bool{}}
	for _, id := range showtimes {
		s.showtimes[id] = true
	}
	return s
}

// Scan checks a QR code locally. Only accepted scans are queued for sync.
func (s *Scanner) Scan(code string, now time.Time) (string, ticket.Claims) {
	c, err := ticket.Verify(s.pub, code, now)
	switch {
	case errors.Is(err, ticket.ErrSignature):
		return RejectSignature, c
	case errors.Is(err, ticket.ErrNotYet):
		return RejectNotYet, c
	case errors.Is(err, ticket.ErrExpired):
		return RejectExpired, c
	case err != nil:
		return RejectMalformed, c
	}
	if len(s.showtimes) > 0 && !s.showtimes[c.ShowtimeID] {
		return RejectWrongShow, c
	}
	if s.revoked[c.TicketID] {
		return RejectRefunded, c
	}
	if s.used[c.TicketID] {
		return RejectUsed, c
	}
	s.used[c.TicketID] = true
	s.pending = append(s.pending, Event{TicketID: c.TicketID, GateID: s.GateID, ScannedAt: now})
	return Accepted, c
}

// Pending returns scans not yet acknowledged by the server.
func (s *Scanner) Pending() []Event { return append([]Event(nil), s.pending...) }

// SyncState is what the server sends back to a scanner.
type SyncState struct {
	Used    []string `json:"used"`    // tickets used at any gate
	Revoked []string `json:"revoked"` // refunded tickets
}

// ApplySync clears the uploaded queue and merges the server's view so that
// a ticket used at another gate or refunded meanwhile is rejected here too.
func (s *Scanner) ApplySync(st SyncState) {
	s.pending = nil
	for _, id := range st.Used {
		s.used[id] = true
	}
	for _, id := range st.Revoked {
		s.revoked[id] = true
	}
}

// Ticket statuses on the server.
const (
	StatusValid    = "valid"
	StatusUsed     = "used"
	StatusRefunded = "refunded"
)

// TicketState is the server's record of a ticket.
type TicketState struct {
	ID     string
	Status string
	UsedAt time.Time
	UsedBy string
}

// Conflict kinds.
const (
	ConflictDuplicate      = "DUPLICATE_ENTRY"         // same ticket admitted twice
	ConflictRefundedEntry  = "ADMITTED_AFTER_REFUND"   // refunded ticket admitted offline
	ConflictUnknownTicket  = "UNKNOWN_TICKET"          // signed, but not in the database
)

// Conflict is reported to the hall manager; it is never silently dropped.
type Conflict struct {
	Kind      string    `json:"kind"`
	TicketID  string    `json:"ticket_id"`
	GateID    string    `json:"gate_id"`
	ScannedAt time.Time `json:"scanned_at"`
	FirstGate string    `json:"first_gate,omitempty"`
	FirstAt   time.Time `json:"first_at,omitempty"`
}

// Reconcile applies uploaded events to the server state. Events are
// processed in scan time order; the earliest scan of a ticket wins, every
// later one is a duplicate. Re-uploading the same event is a no-op, so sync
// is idempotent. It returns the tickets whose state changed and conflicts.
func Reconcile(state map[string]*TicketState, events []Event) (changed []*TicketState, conflicts []Conflict) {
	ev := append([]Event(nil), events...)
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].ScannedAt.Before(ev[j].ScannedAt) })
	touched := map[string]bool{}
	for _, e := range ev {
		t, ok := state[e.TicketID]
		if !ok {
			conflicts = append(conflicts, Conflict{Kind: ConflictUnknownTicket, TicketID: e.TicketID, GateID: e.GateID, ScannedAt: e.ScannedAt})
			continue
		}
		switch t.Status {
		case StatusValid:
			t.Status, t.UsedAt, t.UsedBy = StatusUsed, e.ScannedAt, e.GateID
			touched[t.ID] = true
		case StatusUsed:
			if t.UsedBy == e.GateID && t.UsedAt.Equal(e.ScannedAt) {
				continue // same event uploaded twice
			}
			first := Conflict{Kind: ConflictDuplicate, TicketID: t.ID, GateID: e.GateID, ScannedAt: e.ScannedAt, FirstGate: t.UsedBy, FirstAt: t.UsedAt}
			if e.ScannedAt.Before(t.UsedAt) {
				// an offline scan happened before the recorded one: it is the
				// real first entry, the recorded one becomes the duplicate
				first = Conflict{Kind: ConflictDuplicate, TicketID: t.ID, GateID: t.UsedBy, ScannedAt: t.UsedAt, FirstGate: e.GateID, FirstAt: e.ScannedAt}
				t.UsedAt, t.UsedBy = e.ScannedAt, e.GateID
				touched[t.ID] = true
			}
			conflicts = append(conflicts, first)
		case StatusRefunded:
			conflicts = append(conflicts, Conflict{Kind: ConflictRefundedEntry, TicketID: t.ID, GateID: e.GateID, ScannedAt: e.ScannedAt})
		}
	}
	ids := make([]string, 0, len(touched))
	for id := range touched {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		changed = append(changed, state[id])
	}
	return changed, conflicts
}
