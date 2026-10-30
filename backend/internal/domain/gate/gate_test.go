package gate

import (
	"crypto/ed25519"
	"testing"
	"time"

	"sahnebilet/internal/domain/ticket"
)

var t0 = time.Date(2026, 10, 30, 19, 30, 0, 0, time.UTC)

func setup(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, priv.Public().(ed25519.PublicKey)
}

func code(t *testing.T, priv ed25519.PrivateKey, id string, show int64) string {
	t.Helper()
	c, err := ticket.Sign(priv, ticket.Claims{TicketID: id, ShowtimeID: show, Seat: "A-1",
		NotBefore: t0.Add(-time.Hour).Unix(), NotAfter: t0.Add(3 * time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestScanner_OfflineSingleUse(t *testing.T) {
	priv, pub := setup(t)
	s := NewScanner("G1", pub, 7)
	c := code(t, priv, "t1", 7)
	if r, _ := s.Scan(c, t0); r != Accepted {
		t.Fatalf("first scan %s", r)
	}
	if r, _ := s.Scan(c, t0.Add(time.Minute)); r != RejectUsed {
		t.Fatalf("second scan %s", r)
	}
	if len(s.Pending()) != 1 {
		t.Fatalf("pending %d", len(s.Pending()))
	}
}

func TestScanner_Rejections(t *testing.T) {
	priv, pub := setup(t)
	s := NewScanner("G1", pub, 7)
	if r, _ := s.Scan(code(t, priv, "t2", 8), t0); r != RejectWrongShow {
		t.Fatalf("wrong show: %s", r)
	}
	if r, _ := s.Scan("garbage", t0); r != RejectMalformed {
		t.Fatalf("garbage: %s", r)
	}
	if r, _ := s.Scan(code(t, priv, "t3", 7), t0.Add(-3*time.Hour)); r != RejectNotYet {
		t.Fatalf("early: %s", r)
	}
	if r, _ := s.Scan(code(t, priv, "t3", 7), t0.Add(4*time.Hour)); r != RejectExpired {
		t.Fatalf("late: %s", r)
	}
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 9
	forger := ed25519.NewKeyFromSeed(seed)
	if r, _ := s.Scan(code(t, forger, "t4", 7), t0); r != RejectSignature {
		t.Fatalf("forged: %s", r)
	}
	if len(s.Pending()) != 0 {
		t.Fatal("rejected scans must not be queued")
	}
}

func TestScanner_SyncPropagatesUsedAndRevoked(t *testing.T) {
	priv, pub := setup(t)
	s := NewScanner("G2", pub)
	s.Scan(code(t, priv, "mine", 7), t0)
	s.ApplySync(SyncState{Used: []string{"usedElsewhere"}, Revoked: []string{"refunded"}})
	if len(s.Pending()) != 0 {
		t.Fatal("sync must clear the queue")
	}
	if r, _ := s.Scan(code(t, priv, "usedElsewhere", 7), t0); r != RejectUsed {
		t.Fatalf("got %s", r)
	}
	if r, _ := s.Scan(code(t, priv, "refunded", 7), t0); r != RejectRefunded {
		t.Fatalf("got %s", r)
	}
}

func TestReconcile_TwoOfflineGatesSameTicket(t *testing.T) {
	priv, pub := setup(t)
	g1, g2 := NewScanner("G1", pub), NewScanner("G2", pub)
	c := code(t, priv, "dup", 7)
	// network is down: both gates admit the same ticket
	g1.Scan(c, t0.Add(2*time.Minute))
	g2.Scan(c, t0.Add(1*time.Minute))
	state := map[string]*TicketState{"dup": {ID: "dup", Status: StatusValid}}

	changed, conf := Reconcile(state, g1.Pending())
	if len(changed) != 1 || len(conf) != 0 || state["dup"].UsedBy != "G1" {
		t.Fatalf("first sync: %+v %+v", changed, conf)
	}
	// G2 scanned earlier: it is the real first entry, G1 becomes duplicate
	changed, conf = Reconcile(state, g2.Pending())
	if len(conf) != 1 || conf[0].Kind != ConflictDuplicate || conf[0].GateID != "G1" || conf[0].FirstGate != "G2" {
		t.Fatalf("conflict: %+v", conf)
	}
	if state["dup"].UsedBy != "G2" || len(changed) != 1 {
		t.Fatalf("state %+v", state["dup"])
	}
}

func TestReconcile_SameBatchOrderAndIdempotent(t *testing.T) {
	state := map[string]*TicketState{"a": {ID: "a", Status: StatusValid}}
	ev := []Event{
		{TicketID: "a", GateID: "G2", ScannedAt: t0.Add(time.Minute)},
		{TicketID: "a", GateID: "G1", ScannedAt: t0},
	}
	_, conf := Reconcile(state, ev)
	if state["a"].UsedBy != "G1" || len(conf) != 1 || conf[0].GateID != "G2" {
		t.Fatalf("%+v %+v", state["a"], conf)
	}
	changed, conf := Reconcile(state, ev[1:])
	if len(changed) != 0 || len(conf) != 0 {
		t.Fatalf("re-upload must be a no-op: %+v %+v", changed, conf)
	}
}

func TestReconcile_RefundedAndUnknown(t *testing.T) {
	state := map[string]*TicketState{"r": {ID: "r", Status: StatusRefunded}}
	_, conf := Reconcile(state, []Event{
		{TicketID: "r", GateID: "G1", ScannedAt: t0},
		{TicketID: "ghost", GateID: "G1", ScannedAt: t0},
	})
	if len(conf) != 2 {
		t.Fatalf("%+v", conf)
	}
	kinds := map[string]bool{conf[0].Kind: true, conf[1].Kind: true}
	if !kinds[ConflictRefundedEntry] || !kinds[ConflictUnknownTicket] {
		t.Fatalf("%+v", conf)
	}
	if state["r"].Status != StatusRefunded {
		t.Fatal("refunded ticket must stay refunded")
	}
}
