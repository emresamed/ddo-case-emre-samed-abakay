// Package seating implements the single-seat (orphan seat) rule and the
// group seat suggestion algorithm (task T-2). It is pure: no I/O, no clock.
package seating

import (
	"errors"
	"math"
	"sort"
)

// Seat kinds. A blocked seat is not sellable and behaves like an aisle/wall.
const (
	KindStandard   = "standard"
	KindVIP        = "vip"
	KindAccessible = "accessible"
	KindBlocked    = "blocked"
)

// Seat is the state of one seat of one showtime.
type Seat struct {
	ID    int64
	Row   int // 1 = front row (closest to the screen)
	Col   int // 1 = leftmost seat looking at the screen
	Kind  string
	Taken bool // sold, or held by an active (non-expired) hold
}

// Violation is a free seat that would be left alone between two taken seats
// (or a taken seat and a wall) if the selection were accepted.
type Violation struct {
	SeatID int64 `json:"seat_id"`
	Row    int   `json:"row"`
	Col    int   `json:"col"`
}

var (
	ErrUnknownSeat  = errors.New("seat does not belong to this showtime")
	ErrSeatTaken    = errors.New("seat is already taken")
	ErrSeatBlocked  = errors.New("seat is blocked")
	ErrInvalidGroup = errors.New("group size must be between 1 and 10")
)

// runs splits the hall into maximal horizontal runs of sellable, physically
// adjacent seats. A missing column or a blocked seat ends a run.
func runs(seats []Seat) [][]Seat {
	byRow := map[int][]Seat{}
	for _, s := range seats {
		byRow[s.Row] = append(byRow[s.Row], s)
	}
	rows := make([]int, 0, len(byRow))
	for r := range byRow {
		rows = append(rows, r)
	}
	sort.Ints(rows)

	var out [][]Seat
	for _, r := range rows {
		row := byRow[r]
		sort.Slice(row, func(i, j int) bool { return row[i].Col < row[j].Col })
		var cur []Seat
		for _, s := range row {
			if s.Kind == KindBlocked {
				if len(cur) > 0 {
					out = append(out, cur)
				}
				cur = nil
				continue
			}
			if len(cur) > 0 && s.Col != cur[len(cur)-1].Col+1 {
				out = append(out, cur)
				cur = nil
			}
			cur = append(cur, s)
		}
		if len(cur) > 0 {
			out = append(out, cur)
		}
	}
	return out
}

// orphans returns the IDs of free seats that are isolated: both neighbours
// are taken or a wall. A run of length 1 is structural and never an orphan.
func orphans(seats []Seat) map[int64]Violation {
	out := map[int64]Violation{}
	for _, run := range runs(seats) {
		if len(run) < 2 {
			continue
		}
		for i, s := range run {
			if s.Taken {
				continue
			}
			left := i == 0 || run[i-1].Taken
			right := i == len(run)-1 || run[i+1].Taken
			if left && right {
				out[s.ID] = Violation{SeatID: s.ID, Row: s.Row, Col: s.Col}
			}
		}
	}
	return out
}

// CheckSingleSeat validates a selection against the current state. It
// returns an error if a selected seat is unknown, taken or blocked, and the
// list of orphan seats the selection would newly create. Orphans that already
// existed before the selection are not blamed on it.
func CheckSingleSeat(seats []Seat, selected []int64) ([]Violation, error) {
	idx := make(map[int64]int, len(seats))
	for i, s := range seats {
		idx[s.ID] = i
	}
	after := make([]Seat, len(seats))
	copy(after, seats)
	for _, id := range selected {
		i, ok := idx[id]
		if !ok {
			return nil, ErrUnknownSeat
		}
		if seats[i].Kind == KindBlocked {
			return nil, ErrSeatBlocked
		}
		if seats[i].Taken {
			return nil, ErrSeatTaken
		}
		after[i].Taken = true
	}

	before := orphans(seats)
	var out []Violation
	for id, v := range orphans(after) {
		if _, existed := before[id]; !existed {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].Col < out[j].Col
	})
	return out, nil
}

// Suggestion is a block of adjacent seats for a group.
type Suggestion struct {
	SeatIDs []int64 `json:"seat_ids"`
	Row     int     `json:"row"`
	FromCol int     `json:"from_col"`
	ToCol   int     `json:"to_col"`
	Score   float64 `json:"score"` // lower is better
}

// Options tune SuggestGroup.
type Options struct {
	// Accessible requests a block that contains at least one accessible
	// seat. When false, accessible seats are never offered: they stay
	// reserved for wheelchair users.
	Accessible bool
	Limit      int
}

// idealRow is the row with the best view: about 60% of the way back.
func idealRow(maxRow int) float64 {
	return math.Max(1, math.Round(float64(maxRow)*0.6))
}

// SuggestGroup returns the best blocks of n adjacent free seats, ordered by
// distance to the ideal viewing point. Blocks that would leave an orphan
// seat are skipped, so every suggestion can be sold without an override.
func SuggestGroup(seats []Seat, n int, opt Options) ([]Suggestion, error) {
	if n < 1 || n > 10 {
		return nil, ErrInvalidGroup
	}
	if opt.Limit <= 0 {
		opt.Limit = 3
	}
	maxRow := 0
	rowMin, rowMax := map[int]int{}, map[int]int{}
	for _, s := range seats {
		if s.Row > maxRow {
			maxRow = s.Row
		}
		if v, ok := rowMin[s.Row]; !ok || s.Col < v {
			rowMin[s.Row] = s.Col
		}
		if s.Col > rowMax[s.Row] {
			rowMax[s.Row] = s.Col
		}
	}
	ideal := idealRow(maxRow)

	var out []Suggestion
	for _, run := range runs(seats) {
		for start := 0; start+n <= len(run); start++ {
			block := run[start : start+n]
			ok, hasAccessible := true, false
			for _, s := range block {
				if s.Taken {
					ok = false
					break
				}
				if s.Kind == KindAccessible {
					hasAccessible = true
				}
			}
			if !ok || hasAccessible != opt.Accessible {
				continue
			}
			ids := make([]int64, n)
			for i, s := range block {
				ids[i] = s.ID
			}
			if v, err := CheckSingleSeat(seats, ids); err != nil || len(v) > 0 {
				continue
			}
			row := block[0].Row
			center := float64(rowMin[row]+rowMax[row]) / 2
			mid := float64(block[0].Col+block[n-1].Col) / 2
			score := math.Abs(float64(row)-ideal) + 0.5*math.Abs(mid-center)
			out = append(out, Suggestion{
				SeatIDs: ids, Row: row, FromCol: block[0].Col, ToCol: block[n-1].Col,
				Score: math.Round(score*100) / 100,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].FromCol < out[j].FromCol
	})
	if len(out) > opt.Limit {
		out = out[:opt.Limit]
	}
	return out, nil
}
