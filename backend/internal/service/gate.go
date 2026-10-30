package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"sahnebilet/internal/domain/gate"
	"sahnebilet/internal/domain/ticket"
)

type ScanResult struct {
	Result    string        `json:"result"`
	TicketID  string        `json:"ticket_id,omitempty"`
	Seat      string        `json:"seat,omitempty"`
	Showtime  int64         `json:"showtime_id,omitempty"`
	FirstGate string        `json:"first_gate,omitempty"`
	FirstAt   *time.Time    `json:"first_at,omitempty"`
}

func validGateID(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && len(id) <= 40
}

// Scan is the online path: verify the signature, then mark the ticket used
// with a conditional UPDATE so that two gates can never both admit it.
func (s *Service) Scan(ctx context.Context, a Actor, gateID, code string) (ScanResult, error) {
	if !validGateID(gateID) {
		return ScanResult{}, invalid("gate_id is required (max 40 chars)")
	}
	now := s.Now()
	c, err := ticket.Verify(s.PublicKey(), code, now)
	res := ScanResult{TicketID: c.TicketID, Seat: c.Seat, Showtime: c.ShowtimeID}
	switch {
	case errors.Is(err, ticket.ErrSignature):
		res.Result = gate.RejectSignature
	case errors.Is(err, ticket.ErrNotYet):
		res.Result = gate.RejectNotYet
	case errors.Is(err, ticket.ErrExpired):
		res.Result = gate.RejectExpired
	case err != nil:
		res.Result = gate.RejectMalformed
	}
	if res.Result != "" {
		gateScans.WithLabelValues(res.Result).Inc()
		return res, nil
	}

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE order_lines SET status = 'used', used_at = $2, used_gate = $3
			WHERE ticket_id::text = $1 AND status = 'valid'`, c.TicketID, now, gateID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			res.Result = gate.Accepted
		} else {
			var status string
			var usedAt *time.Time
			var usedGate *string
			err := tx.QueryRow(ctx, `SELECT status, used_at, used_gate FROM order_lines WHERE ticket_id::text = $1`, c.TicketID).Scan(&status, &usedAt, &usedGate)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				res.Result = gate.RejectMalformed
			case err != nil:
				return err
			case status == "refunded":
				res.Result = gate.RejectRefunded
			default:
				res.Result = gate.RejectUsed
				res.FirstAt = usedAt
				if usedGate != nil {
					res.FirstGate = *usedGate
				}
			}
		}
		if res.Result == gate.RejectMalformed {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO gate_scans (ticket_id, gate_id, scanned_at, offline, result) VALUES ($1::uuid,$2,$3,false,$4)
			ON CONFLICT DO NOTHING`, c.TicketID, gateID, now, res.Result)
		return err
	})
	if err != nil {
		return ScanResult{}, err
	}
	gateScans.WithLabelValues(res.Result).Inc()
	return res, nil
}

// GateState is the data a scanner downloads before going offline.
type GateState struct {
	PublicKey string          `json:"public_key"` // base64 raw Ed25519 public key
	gate.SyncState
	Conflicts []gate.Conflict `json:"conflicts"`
}

// State returns used and refunded tickets of shows around now, so a scanner
// that goes offline already knows them.
func (s *Service) State(ctx context.Context) (GateState, error) {
	rows, err := s.DB.Query(ctx, `SELECT l.ticket_id::text, l.status FROM order_lines l JOIN showtimes st ON st.id = l.showtime_id
		WHERE l.kind = 'ticket' AND l.status <> 'valid' AND st.ends_at > now() - interval '1 hour' AND st.starts_at < now() + interval '1 day'`)
	if err != nil {
		return GateState{}, err
	}
	st := GateState{PublicKey: b64std(s.PublicKey()), Conflicts: []gate.Conflict{}}
	st.Used, st.Revoked = []string{}, []string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return GateState{}, err
		}
		if status == "used" {
			st.Used = append(st.Used, id)
		} else {
			st.Revoked = append(st.Revoked, id)
		}
	}
	rows.Close()
	return st, rows.Err()
}

// SyncRequest is uploaded by a scanner after it comes back online.
type SyncRequest struct {
	GateID string       `json:"gate_id"`
	Scans  []gate.Event `json:"scans"`
}

// Sync merges offline scans. Tickets are locked in id order; reconciliation
// is the pure gate.Reconcile function; conflicts are stored for the hall
// manager and returned to the device.
func (s *Service) Sync(ctx context.Context, a Actor, req SyncRequest) (GateState, error) {
	if !validGateID(req.GateID) {
		return GateState{}, invalid("gate_id is required (max 40 chars)")
	}
	if len(req.Scans) > 5000 {
		return GateState{}, invalid("at most 5000 scans per sync")
	}
	var conflicts []gate.Conflict
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		idSet := map[string]bool{}
		for i := range req.Scans {
			req.Scans[i].GateID = req.GateID
			// PostgreSQL stores microseconds; normalise so replays compare equal
			req.Scans[i].ScannedAt = req.Scans[i].ScannedAt.Truncate(time.Microsecond)
			if req.Scans[i].ScannedAt.IsZero() || req.Scans[i].TicketID == "" {
				return invalid("every scan needs ticket_id and scanned_at")
			}
			idSet[req.Scans[i].TicketID] = true
		}
		ids := make([]string, 0, len(idSet))
		for id := range idSet {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		rows, err := tx.Query(ctx, `SELECT ticket_id::text, status, used_at, COALESCE(used_gate,'') FROM order_lines
			WHERE ticket_id::text = ANY($1) ORDER BY ticket_id FOR UPDATE`, ids)
		if err != nil {
			return err
		}
		state := map[string]*gate.TicketState{}
		for rows.Next() {
			var t gate.TicketState
			var usedAt *time.Time
			if err := rows.Scan(&t.ID, &t.Status, &usedAt, &t.UsedBy); err != nil {
				rows.Close()
				return err
			}
			if usedAt != nil {
				t.UsedAt = *usedAt
			}
			state[t.ID] = &t
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// drop events this server already received (a device retrying a
		// sync after a timeout) so that a replay creates no new conflicts
		seen := map[string]bool{}
		srows, err := tx.Query(ctx, `SELECT ticket_id::text, gate_id, scanned_at FROM gate_scans WHERE ticket_id::text = ANY($1)`, ids)
		if err != nil {
			return err
		}
		for srows.Next() {
			var tid, gid string
			var at time.Time
			if err := srows.Scan(&tid, &gid, &at); err != nil {
				srows.Close()
				return err
			}
			seen[tid+"|"+gid+"|"+at.UTC().Format(time.RFC3339Nano)] = true
		}
		srows.Close()
		fresh := req.Scans[:0:0]
		for _, e := range req.Scans {
			if !seen[e.TicketID+"|"+e.GateID+"|"+e.ScannedAt.UTC().Format(time.RFC3339Nano)] {
				fresh = append(fresh, e)
			}
		}
		req.Scans = fresh
		changed, conf := gate.Reconcile(state, req.Scans)
		conflicts = conf
		for _, t := range changed {
			if _, err := tx.Exec(ctx, `UPDATE order_lines SET status = 'used', used_at = $2, used_gate = $3 WHERE ticket_id::text = $1`,
				t.ID, t.UsedAt, t.UsedBy); err != nil {
				return err
			}
		}
		for _, e := range req.Scans {
			if state[e.TicketID] == nil {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO gate_scans (ticket_id, gate_id, scanned_at, offline, result) VALUES ($1::uuid,$2,$3,true,'ACCEPTED_OFFLINE')
				ON CONFLICT DO NOTHING`, e.TicketID, e.GateID, e.ScannedAt); err != nil {
				return err
			}
		}
		for _, c := range conf {
			if c.Kind == gate.ConflictUnknownTicket {
				continue // not a uuid we know; nothing to store against
			}
			var firstAt any
			if !c.FirstAt.IsZero() {
				firstAt = c.FirstAt
			}
			if _, err := tx.Exec(ctx, `INSERT INTO gate_conflicts (kind, ticket_id, gate_id, scanned_at, first_gate, first_at) VALUES ($1,$2::uuid,$3,$4,NULLIF($5,''),$6)`,
				c.Kind, c.TicketID, c.GateID, c.ScannedAt, c.FirstGate, firstAt); err != nil {
				return err
			}
		}
		if len(conf) > 0 {
			return audit(ctx, tx, a.UserID, "gate.sync_conflicts", "gate", req.GateID, nil, conf)
		}
		return nil
	})
	if err != nil {
		return GateState{}, err
	}
	st, err := s.State(ctx)
	st.Conflicts = conflicts
	if st.Conflicts == nil {
		st.Conflicts = []gate.Conflict{}
	}
	return st, err
}

// ListConflicts returns recent gate conflicts.
func (s *Service) ListConflicts(ctx context.Context) ([]gate.Conflict, error) {
	rows, err := s.DB.Query(ctx, `SELECT kind, ticket_id::text, gate_id, scanned_at, COALESCE(first_gate,''), COALESCE(first_at, 'epoch'::timestamptz)
		FROM gate_conflicts ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (gate.Conflict, error) {
		var c gate.Conflict
		err := r.Scan(&c.Kind, &c.TicketID, &c.GateID, &c.ScannedAt, &c.FirstGate, &c.FirstAt)
		if c.FirstAt.Unix() == 0 {
			c.FirstAt = time.Time{}
		}
		return c, err
	})
	if out == nil {
		out = []gate.Conflict{}
	}
	return out, err
}
