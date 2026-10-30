package service

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// seatChannel is the PostgreSQL NOTIFY channel for seat map changes. The
// payload is the showtime id. NOTIFY is sent inside the changing
// transaction, so it is delivered only if (and when) the change commits,
// and it reaches every API replica.
const seatChannel = "seat_changes"

// broker fans out seat change notifications to SSE subscribers.
type broker struct {
	mu   sync.Mutex
	subs map[int64]map[chan struct{}]struct{}
}

func newBroker() *broker { return &broker{subs: map[int64]map[chan struct{}]struct{}{}} }

func (b *broker) publish(showtimeID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[showtimeID] {
		select {
		case ch <- struct{}{}:
		default: // a change is already pending for this subscriber
		}
	}
}

// Subscribe returns a channel that receives a signal whenever the seat map of
// the showtime changes, and a function to unsubscribe.
func (s *Service) Subscribe(showtimeID int64) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b := s.live
	b.mu.Lock()
	if b.subs[showtimeID] == nil {
		b.subs[showtimeID] = map[chan struct{}]struct{}{}
	}
	b.subs[showtimeID][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs[showtimeID], ch)
		if len(b.subs[showtimeID]) == 0 {
			delete(b.subs, showtimeID)
		}
		b.mu.Unlock()
	}
}

// notifySeats queues a notification in the caller's transaction.
func notifySeats(ctx context.Context, tx pgx.Tx, showtimeID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, seatChannel, strconv.FormatInt(showtimeID, 10))
	return err
}

// RunSeatListener holds one connection in LISTEN mode and forwards
// notifications to subscribers. It reconnects with backoff until ctx ends.
func (s *Service) RunSeatListener(ctx context.Context, logErr func(error)) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := s.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		logErr(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *Service) listen(ctx context.Context) error {
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+seatChannel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			conn.Conn().Close(context.Background()) //nolint:errcheck // broken connection is discarded
			return err
		}
		if id, err := strconv.ParseInt(n.Payload, 10, 64); err == nil {
			s.live.publish(id)
		}
	}
}
