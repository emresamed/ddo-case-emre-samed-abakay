package seating

import (
	"errors"
	"testing"
)

// grid builds rows x cols seats with IDs row*100+col. taken lists
// [row,col] pairs; kinds overrides the kind of specific seats.
func grid(rows, cols int, taken [][2]int, kinds map[[2]int]string) []Seat {
	t := map[[2]int]bool{}
	for _, p := range taken {
		t[p] = true
	}
	var out []Seat
	for r := 1; r <= rows; r++ {
		for c := 1; c <= cols; c++ {
			k := KindStandard
			if v, ok := kinds[[2]int{r, c}]; ok {
				k = v
			}
			out = append(out, Seat{ID: int64(r*100 + c), Row: r, Col: c, Kind: k, Taken: t[[2]int{r, c}]})
		}
	}
	return out
}

func TestCheckSingleSeat_LeavesGapAtWall(t *testing.T) {
	seats := grid(1, 6, nil, nil)
	v, err := CheckSingleSeat(seats, []int64{102, 103})
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 || v[0].SeatID != 101 {
		t.Fatalf("expected seat 101 orphaned, got %+v", v)
	}
}

func TestCheckSingleSeat_LeavesGapBetweenTaken(t *testing.T) {
	seats := grid(1, 6, [][2]int{{1, 1}}, nil)
	v, _ := CheckSingleSeat(seats, []int64{103, 104})
	if len(v) != 1 || v[0].SeatID != 102 {
		t.Fatalf("expected seat 102 orphaned, got %+v", v)
	}
}

func TestCheckSingleSeat_Valid(t *testing.T) {
	seats := grid(1, 6, nil, nil)
	v, err := CheckSingleSeat(seats, []int64{101, 102})
	if err != nil || len(v) != 0 {
		t.Fatalf("expected valid, got %v %+v", err, v)
	}
	v, _ = CheckSingleSeat(seats, []int64{103, 104})
	if len(v) != 0 {
		t.Fatalf("two free seats on each side are fine, got %+v", v)
	}
}

func TestCheckSingleSeat_FillingGapIsAllowed(t *testing.T) {
	seats := grid(1, 5, [][2]int{{1, 1}, {1, 3}}, nil)
	v, err := CheckSingleSeat(seats, []int64{102})
	if err != nil || len(v) != 0 {
		t.Fatalf("filling an existing orphan must be allowed, got %v %+v", err, v)
	}
}

func TestCheckSingleSeat_PreexistingOrphanNotBlamed(t *testing.T) {
	// seat 2 is already an orphan; selecting 7,8 must not be rejected for it.
	seats := grid(1, 8, [][2]int{{1, 1}, {1, 3}}, nil)
	v, _ := CheckSingleSeat(seats, []int64{107, 108})
	if len(v) != 0 {
		t.Fatalf("got %+v", v)
	}
}

func TestCheckSingleSeat_BlockedActsAsWall(t *testing.T) {
	seats := grid(1, 6, nil, map[[2]int]string{{1, 3}: KindBlocked})
	v, _ := CheckSingleSeat(seats, []int64{101})
	if len(v) != 1 || v[0].SeatID != 102 {
		t.Fatalf("seat 102 sits between taken 101 and blocked 103, got %+v", v)
	}
}

func TestCheckSingleSeat_LoneStructuralSeatIgnored(t *testing.T) {
	seats := grid(1, 3, nil, map[[2]int]string{{1, 2}: KindBlocked})
	v, _ := CheckSingleSeat(seats, []int64{103})
	if len(v) != 0 {
		t.Fatalf("seat 101 is a one-seat run, never an orphan; got %+v", v)
	}
}

func TestCheckSingleSeat_Errors(t *testing.T) {
	seats := grid(1, 4, [][2]int{{1, 1}}, map[[2]int]string{{1, 4}: KindBlocked})
	if _, err := CheckSingleSeat(seats, []int64{999}); !errors.Is(err, ErrUnknownSeat) {
		t.Fatalf("want ErrUnknownSeat, got %v", err)
	}
	if _, err := CheckSingleSeat(seats, []int64{101}); !errors.Is(err, ErrSeatTaken) {
		t.Fatalf("want ErrSeatTaken, got %v", err)
	}
	if _, err := CheckSingleSeat(seats, []int64{104}); !errors.Is(err, ErrSeatBlocked) {
		t.Fatalf("want ErrSeatBlocked, got %v", err)
	}
}

func TestSuggestGroup_PrefersCenter(t *testing.T) {
	seats := grid(10, 12, nil, nil)
	s, err := SuggestGroup(seats, 4, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s) == 0 {
		t.Fatal("no suggestion")
	}
	best := s[0]
	if best.Row != 6 {
		t.Fatalf("ideal row for 10 rows is 6, got %d", best.Row)
	}
	if best.FromCol != 5 || best.ToCol != 8 {
		t.Fatalf("expected centered cols 5-8, got %d-%d", best.FromCol, best.ToCol)
	}
}

func TestSuggestGroup_SkipsTakenAndOrphanCreating(t *testing.T) {
	// Row 1 of 6: seat 1 taken. A 4-block at 2-5 leaves seat 6 alone,
	// 3-6 leaves seat 2 alone; so no block of 4 fits without violation.
	seats := grid(1, 6, [][2]int{{1, 1}}, nil)
	s, _ := SuggestGroup(seats, 4, Options{})
	if len(s) != 0 {
		t.Fatalf("expected none, got %+v", s)
	}
	s, _ = SuggestGroup(seats, 5, Options{})
	if len(s) != 1 || s[0].FromCol != 2 {
		t.Fatalf("expected 2-6, got %+v", s)
	}
}

func TestSuggestGroup_AccessibleSeparated(t *testing.T) {
	kinds := map[[2]int]string{{3, 5}: KindAccessible, {3, 6}: KindAccessible}
	seats := grid(5, 10, nil, kinds)
	normal, _ := SuggestGroup(seats, 2, Options{Limit: 50})
	for _, sg := range normal {
		for _, id := range sg.SeatIDs {
			if id == 305 || id == 306 {
				t.Fatalf("accessible seat offered to a normal group: %+v", sg)
			}
		}
	}
	acc, _ := SuggestGroup(seats, 2, Options{Accessible: true, Limit: 50})
	if len(acc) == 0 {
		t.Fatal("expected an accessible suggestion")
	}
	for _, sg := range acc {
		found := false
		for _, id := range sg.SeatIDs {
			if id == 305 || id == 306 {
				found = true
			}
		}
		if !found {
			t.Fatalf("accessible suggestion without accessible seat: %+v", sg)
		}
	}
}

func TestSuggestGroup_InvalidSize(t *testing.T) {
	if _, err := SuggestGroup(grid(2, 2, nil, nil), 0, Options{}); !errors.Is(err, ErrInvalidGroup) {
		t.Fatal("want ErrInvalidGroup")
	}
	if _, err := SuggestGroup(grid(2, 2, nil, nil), 11, Options{}); !errors.Is(err, ErrInvalidGroup) {
		t.Fatal("want ErrInvalidGroup")
	}
}

func TestSuggestGroup_Limit(t *testing.T) {
	s, _ := SuggestGroup(grid(8, 10, nil, nil), 2, Options{Limit: 5})
	if len(s) != 5 {
		t.Fatalf("want 5, got %d", len(s))
	}
	for i := 1; i < len(s); i++ {
		if s[i].Score < s[i-1].Score {
			t.Fatal("not sorted by score")
		}
	}
}
