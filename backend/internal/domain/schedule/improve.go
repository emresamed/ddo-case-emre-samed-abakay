package schedule

import (
	"sort"
	"time"
)

// Stats reports what the improvement round achieved.
type Stats struct {
	ObjectiveBefore int64 `json:"objective_before"`
	ObjectiveAfter  int64 `json:"objective_after"`
	Swaps           int   `json:"swaps"`
	Passes          int   `json:"passes"`
}

// primeWeight makes a seat in prime time worth twice a seat at noon.
const primeWeight = 2

// Objective scores how well demand meets capacity: for every show, hall
// seats x movie demand, doubled in prime time. Higher is better: popular
// films in big halls at the hours people come.
func Objective(cfg Config, halls []Hall, movies []Movie, plan []Show) int64 {
	seats, demand := index(halls, movies)
	var sum int64
	for _, s := range plan {
		w := int64(1)
		if cfg.prime(s.Start) {
			w = primeWeight
		}
		sum += w * int64(seats[s.HallID]) * int64(demand[s.MovieID])
	}
	return sum
}

func index(halls []Hall, movies []Movie) (map[int64]int, map[int64]int) {
	seats := map[int64]int{}
	for _, h := range halls {
		seats[h.ID] = h.Seats
	}
	demand := map[int64]int{}
	for _, m := range movies {
		demand[m.ID] = m.Demand
	}
	return seats, demand
}

// Improve is a local search run after Generate. It swaps the films of two
// shows in different halls when the swap moves the more popular film into
// the bigger hall. Only shows on the same day with the same prime-time
// status are swapped, so every film keeps its weekly count and its prime
// share: contract constraints cannot break. A swap is applied only if both
// films still fit before the next show in their new hall (ads + film +
// cleaning) and before closing. Passes repeat until nothing improves.
// The result is deterministic.
func Improve(cfg Config, halls []Hall, movies []Movie, plan []Show) ([]Show, Stats) {
	out := append([]Show(nil), plan...)
	seats, demand := index(halls, movies)
	dur := map[int64]int{}
	for _, m := range movies {
		dur[m.ID] = m.DurationMin
	}
	st := Stats{ObjectiveBefore: Objective(cfg, halls, movies, out)}

	// position of each show in its hall's timeline, to find the next show
	byHall := map[int64][]int{}
	reindex := func() {
		byHall = map[int64][]int{}
		for i, s := range out {
			byHall[s.HallID] = append(byHall[s.HallID], i)
		}
		for _, ids := range byHall {
			sort.Slice(ids, func(a, b int) bool { return out[ids[a]].Start.Before(out[ids[b]].Start) })
		}
	}
	nextStart := func(i int) (time.Time, bool) {
		ids := byHall[out[i].HallID]
		for k, id := range ids {
			if id == i && k+1 < len(ids) {
				return out[ids[k+1]].Start, true
			}
		}
		return time.Time{}, false
	}
	fits := func(i int, movie int64) (time.Time, bool) {
		s := out[i]
		end := s.Start.Add(time.Duration(cfg.AdsMin+dur[movie]) * time.Minute)
		free := end.Add(time.Duration(cfg.CleaningMin) * time.Minute)
		// closing time always applies: for the last show of the day the
		// "next show" is tomorrow morning, which would allow running past 01:00
		day := time.Date(s.Start.Year(), s.Start.Month(), s.Start.Day(), 0, 0, 0, 0, s.Start.Location())
		if int(free.Sub(day).Minutes()) > cfg.CloseMin {
			return end, false
		}
		if next, ok := nextStart(i); ok {
			return end, !free.After(next)
		}
		return end, true
	}
	dayOf := func(t time.Time) string { return t.Format("2006-01-02") }

	reindex()
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		if !out[order[a]].Start.Equal(out[order[b]].Start) {
			return out[order[a]].Start.Before(out[order[b]].Start)
		}
		return out[order[a]].HallID < out[order[b]].HallID
	})

	for st.Passes = 1; st.Passes <= 6; st.Passes++ {
		improved := false
		for x := 0; x < len(order); x++ {
			i := order[x]
			for y := x + 1; y < len(order); y++ {
				j := order[y]
				a, b := out[i], out[j]
				if dayOf(b.Start) != dayOf(a.Start) {
					break // order is by start time: no more shows on this day
				}
				if a.HallID == b.HallID || a.MovieID == b.MovieID || cfg.prime(a.Start) != cfg.prime(b.Start) {
					continue
				}
				gain := int64(seats[a.HallID]-seats[b.HallID]) * int64(demand[b.MovieID]-demand[a.MovieID])
				if gain <= 0 {
					continue
				}
				endA, okA := fits(i, b.MovieID)
				endB, okB := fits(j, a.MovieID)
				if !okA || !okB {
					continue
				}
				out[i].MovieID, out[i].End = b.MovieID, endA
				out[j].MovieID, out[j].End = a.MovieID, endB
				st.Swaps++
				improved = true
			}
		}
		if !improved {
			break
		}
	}
	if st.Passes > 6 {
		st.Passes = 6
	}
	st.ObjectiveAfter = Objective(cfg, halls, movies, out)
	return out, st
}
