// Package schedule generates a conflict-free showtime plan per hall and
// validates it against distribution contract rules (task T-3).
package schedule

import (
	"fmt"
	"sort"
	"time"
)

// Hall is a screen room.
type Hall struct {
	ID    int64
	Name  string
	Seats int // sellable seats; used by Improve
}

// Movie carries the contract constraints for one film or play.
type Movie struct {
	ID             int64
	Title          string
	DurationMin    int
	MinWeeklyShows int   // contract: at least this many shows per week (all halls)
	MinPrimeBP     int64 // contract: at least this share of its shows in prime time
	Demand         int   // expected popularity (relative); used by Improve
}

// Config describes the operating rules of the cinema.
type Config struct {
	Start        time.Time // local midnight of the first day
	Days         int
	OpenMin      int // first show may start at this minute of the day
	CloseMin     int // last show must end (incl. cleaning) by this minute; may exceed 1440
	AdsMin       int
	CleaningMin  int
	PrimeFromMin int // prime time: start in [PrimeFromMin, PrimeToMin)
	PrimeToMin   int
	StepMin      int // start times are rounded up to this step
}

// DefaultConfig: 10:00 opening, last show done by 01:00, 15 min ads, 20 min
// cleaning, prime time 18:00-22:00.
func DefaultConfig(start time.Time, days int) Config {
	return Config{Start: start, Days: days, OpenMin: 600, CloseMin: 1500, AdsMin: 15, CleaningMin: 20,
		PrimeFromMin: 1080, PrimeToMin: 1320, StepMin: 5}
}

// Show is one planned showtime. End is when the film ends; the hall is busy
// until End + cleaning.
type Show struct {
	HallID  int64     `json:"hall_id"`
	MovieID int64     `json:"movie_id"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
}

func (c Config) prime(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	return m >= c.PrimeFromMin && m < c.PrimeToMin
}

func (c Config) week(t time.Time) int {
	return int(t.Sub(c.Start).Hours()/24) / 7
}

type tally struct{ total, prime int }

// Generate fills every hall from opening to closing, one show after the
// other. At each slot it picks a movie greedily:
//   - in prime time: the movie furthest below its prime-time share,
//   - otherwise: the movie furthest below its weekly minimum, and once all
//     minimums are met, the movie whose prime share can best absorb one
//     more non-prime show.
//
// Halls start a few minutes apart so that crowds do not hit the lobby at once.
func Generate(cfg Config, halls []Hall, movies []Movie) ([]Show, error) {
	if len(halls) == 0 || len(movies) == 0 {
		return nil, fmt.Errorf("need at least one hall and one movie")
	}
	ms := append([]Movie(nil), movies...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].ID < ms[j].ID })
	hs := append([]Hall(nil), halls...)
	sort.Slice(hs, func(i, j int) bool { return hs[i].ID < hs[j].ID })

	var out []Show
	counts := map[int]map[int64]*tally{}
	for d := 0; d < cfg.Days; d++ {
		day := cfg.Start.AddDate(0, 0, d)
		wk := d / 7
		if counts[wk] == nil {
			counts[wk] = map[int64]*tally{}
			for _, m := range ms {
				counts[wk][m.ID] = &tally{}
			}
		}
		cnt := counts[wk]
		// rotate halls so the same hall does not always get first pick
		for hi := range hs {
			h := hs[(hi+d)%len(hs)]
			cursor := cfg.OpenMin + (hi*10)%60
			for {
				start := (cursor + cfg.StepMin - 1) / cfg.StepMin * cfg.StepMin
				st := day.Add(time.Duration(start) * time.Minute)
				inPrime := cfg.prime(st)
				var best *Movie
				var bestKey [3]int64
				for i := range ms {
					m := &ms[i]
					if start+cfg.AdsMin+m.DurationMin+cfg.CleaningMin > cfg.CloseMin {
						continue
					}
					t := cnt[m.ID]
					deficit := int64(m.MinWeeklyShows - t.total)
					var key [3]int64
					if inPrime {
						target := int64(max(t.total+1, m.MinWeeklyShows))
						primeGap := m.MinPrimeBP*target - int64(t.prime)*10000
						key = [3]int64{primeGap, deficit, -int64(t.total)}
					} else {
						headroom := int64(t.prime)*10000 - m.MinPrimeBP*int64(t.total+1)
						key = [3]int64{max(deficit, 0), headroom, -int64(t.total)}
					}
					if best == nil || greater(key, bestKey) {
						best, bestKey = m, key
					}
				}
				if best == nil {
					break
				}
				end := st.Add(time.Duration(cfg.AdsMin+best.DurationMin) * time.Minute)
				out = append(out, Show{HallID: h.ID, MovieID: best.ID, Start: st, End: end})
				cnt[best.ID].total++
				if inPrime {
					cnt[best.ID].prime++
				}
				cursor = start + cfg.AdsMin + best.DurationMin + cfg.CleaningMin
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HallID != out[j].HallID {
			return out[i].HallID < out[j].HallID
		}
		return out[i].Start.Before(out[j].Start)
	})
	return out, nil
}

func greater(a, b [3]int64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// Problem is one violated constraint.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Validate checks a plan independently of how it was produced: hall
// overlaps including ads and cleaning, opening hours, show length, weekly
// minimum shows and prime-time share per movie per week.
func Validate(cfg Config, halls []Hall, movies []Movie, shows []Show) []Problem {
	var p []Problem
	add := func(code, f string, a ...any) { p = append(p, Problem{code, fmt.Sprintf(f, a...)}) }

	hallOK := map[int64]bool{}
	for _, h := range halls {
		hallOK[h.ID] = true
	}
	movie := map[int64]Movie{}
	for _, m := range movies {
		movie[m.ID] = m
	}

	byHall := map[int64][]Show{}
	weeks := (cfg.Days + 6) / 7
	cnt := make([]map[int64]*tally, weeks)
	for w := range cnt {
		cnt[w] = map[int64]*tally{}
		for _, m := range movies {
			cnt[w][m.ID] = &tally{}
		}
	}
	end := cfg.Start.AddDate(0, 0, cfg.Days)
	for _, s := range shows {
		m, ok := movie[s.MovieID]
		if !ok {
			add("UNKNOWN_MOVIE", "show at %s references movie %d", s.Start.Format(time.RFC3339), s.MovieID)
			continue
		}
		if !hallOK[s.HallID] {
			add("UNKNOWN_HALL", "show at %s references hall %d", s.Start.Format(time.RFC3339), s.HallID)
			continue
		}
		if s.Start.Before(cfg.Start) || !s.Start.Before(end) {
			add("OUT_OF_RANGE", "hall %d show at %s is outside the planning period", s.HallID, s.Start.Format(time.RFC3339))
			continue
		}
		if want := s.Start.Add(time.Duration(cfg.AdsMin+m.DurationMin) * time.Minute); !s.End.Equal(want) {
			add("BAD_LENGTH", "hall %d show at %s must end at %s (ads + film)", s.HallID, s.Start.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		day := time.Date(s.Start.Year(), s.Start.Month(), s.Start.Day(), 0, 0, 0, 0, s.Start.Location())
		startMin := int(s.Start.Sub(day).Minutes())
		doneMin := int(s.End.Sub(day).Minutes()) + cfg.CleaningMin
		if startMin < cfg.OpenMin || doneMin > cfg.CloseMin {
			add("OUTSIDE_HOURS", "hall %d show at %s is outside opening hours", s.HallID, s.Start.Format(time.RFC3339))
		}
		byHall[s.HallID] = append(byHall[s.HallID], s)
		t := cnt[cfg.week(s.Start)][s.MovieID]
		t.total++
		if cfg.prime(s.Start) {
			t.prime++
		}
	}

	for hid, list := range byHall {
		sort.Slice(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })
		for i := 1; i < len(list); i++ {
			free := list[i-1].End.Add(time.Duration(cfg.CleaningMin) * time.Minute)
			if list[i].Start.Before(free) {
				add("OVERLAP", "hall %d: show at %s starts before the previous one is cleaned (%s)",
					hid, list[i].Start.Format(time.RFC3339), free.Format(time.RFC3339))
			}
		}
	}

	for w := range cnt {
		// a partial last week is only checked pro rata
		days := min(7, cfg.Days-w*7)
		for _, m := range movies {
			t := cnt[w][m.ID]
			need := (m.MinWeeklyShows*days + 6) / 7
			if t.total < need {
				add("WEEKLY_MIN", "week %d: %q has %d shows, contract requires %d", w+1, m.Title, t.total, need)
			}
			if t.total > 0 && int64(t.prime)*10000 < m.MinPrimeBP*int64(t.total) {
				add("PRIME_SHARE", "week %d: %q has %d/%d prime-time shows, contract requires %d%%",
					w+1, m.Title, t.prime, t.total, m.MinPrimeBP/100)
			}
		}
	}
	sort.SliceStable(p, func(i, j int) bool { return p[i].Code < p[j].Code })
	return p
}
