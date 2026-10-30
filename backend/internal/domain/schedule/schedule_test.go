package schedule

import (
	"fmt"
	"testing"
	"time"
)

var trt = time.FixedZone("TRT", 3*3600)

func fixture() (Config, []Hall, []Movie) {
	cfg := DefaultConfig(time.Date(2026, 11, 2, 0, 0, 0, 0, trt), 14)
	var halls []Hall
	for i := 1; i <= 10; i++ {
		halls = append(halls, Hall{ID: int64(i), Name: "Salon", Seats: 80 + 20*i})
	}
	movies := []Movie{
		{1, "Kuzey Yıldızı", 128, 30, 2500, 30},
		{2, "Son Vapur", 104, 25, 2000, 25},
		{3, "Bozkırda Bir Gece", 142, 20, 2500, 20},
		{4, "Ada Çocukları (Animasyon)", 95, 30, 1000, 30},
		{5, "Kayıp Defter", 117, 20, 2000, 20},
		{6, "Hamlet (Sahne Kaydı)", 165, 8, 3000, 8},
		{7, "Kırmızı Çizgi", 110, 25, 2500, 25},
		{8, "Sessiz Liman", 99, 15, 1500, 15},
	}
	return cfg, halls, movies
}

func TestGenerate_10Halls2Weeks_Valid(t *testing.T) {
	cfg, halls, movies := fixture()
	shows, err := Generate(cfg, halls, movies)
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) < 10*14*4 {
		t.Fatalf("too few shows: %d", len(shows))
	}
	if p := Validate(cfg, halls, movies, shows); len(p) > 0 {
		t.Fatalf("generated plan violates constraints: %+v", p)
	}
}

func TestGenerate_Deterministic(t *testing.T) {
	cfg, halls, movies := fixture()
	a, _ := Generate(cfg, halls, movies)
	b, _ := Generate(cfg, halls, movies)
	if len(a) != len(b) {
		t.Fatal("length differs")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("show %d differs", i)
		}
	}
}

func TestGenerate_NeedsInput(t *testing.T) {
	cfg, halls, _ := fixture()
	if _, err := Generate(cfg, halls, nil); err == nil {
		t.Fatal("expected error")
	}
}

func has(p []Problem, code string) bool {
	for _, x := range p {
		if x.Code == code {
			return true
		}
	}
	return false
}

func TestValidate_DetectsOverlapIncludingCleaning(t *testing.T) {
	cfg, halls, movies := fixture()
	shows, _ := Generate(cfg, halls, movies)
	// move the 2nd show of hall 1 to 5 minutes after the 1st one ends:
	// it is still within the 20 minute cleaning window
	s := shows[1]
	m := movies[s.MovieID-1]
	s.Start = shows[0].End.Add(5 * time.Minute)
	s.End = s.Start.Add(time.Duration(cfg.AdsMin+m.DurationMin) * time.Minute)
	shows[1] = s
	if p := Validate(cfg, halls, movies, shows); !has(p, "OVERLAP") {
		t.Fatalf("overlap not detected: %+v", p)
	}
}

func TestValidate_DetectsContractViolations(t *testing.T) {
	cfg, halls, movies := fixture()
	shows, _ := Generate(cfg, halls, movies)
	// drop every show of movie 1 in week 1
	var kept []Show
	for _, s := range shows {
		if s.MovieID != 1 || cfg.week(s.Start) != 0 {
			kept = append(kept, s)
		}
	}
	if p := Validate(cfg, halls, movies, kept); !has(p, "WEEKLY_MIN") {
		t.Fatalf("weekly minimum not detected: %+v", p)
	}
	// a movie with only non-prime shows breaks the prime share
	only := []Show{{HallID: 1, MovieID: 6, Start: cfg.Start.Add(11 * time.Hour)}}
	only[0].End = only[0].Start.Add(time.Duration(cfg.AdsMin+165) * time.Minute)
	if p := Validate(cfg, halls, movies, only); !has(p, "PRIME_SHARE") {
		t.Fatalf("prime share not detected: %+v", p)
	}
}

func TestValidate_DetectsBadShows(t *testing.T) {
	cfg, halls, movies := fixture()
	early := cfg.Start.Add(8 * time.Hour)
	bad := []Show{
		{HallID: 1, MovieID: 1, Start: early, End: early.Add(143 * time.Minute)},                          // before opening
		{HallID: 1, MovieID: 99, Start: early, End: early},                                                // unknown movie
		{HallID: 99, MovieID: 1, Start: early, End: early},                                                // unknown hall
		{HallID: 2, MovieID: 1, Start: cfg.Start.Add(12 * time.Hour), End: cfg.Start.Add(13 * time.Hour)}, // wrong length
		{HallID: 3, MovieID: 1, Start: cfg.Start.AddDate(0, 0, -1), End: cfg.Start},                       // out of range
	}
	p := Validate(cfg, halls, movies, bad)
	for _, code := range []string{"OUTSIDE_HOURS", "UNKNOWN_MOVIE", "UNKNOWN_HALL", "BAD_LENGTH", "OUT_OF_RANGE"} {
		if !has(p, code) {
			t.Errorf("missing %s in %+v", code, p)
		}
	}
}

func TestImprove_RaisesObjectiveAndKeepsConstraints(t *testing.T) {
	cfg, halls, movies := fixture()
	plan, _ := Generate(cfg, halls, movies)
	better, st := Improve(cfg, halls, movies, plan)
	if st.Swaps == 0 || st.ObjectiveAfter <= st.ObjectiveBefore {
		t.Fatalf("no improvement: %+v", st)
	}
	if st.ObjectiveAfter != Objective(cfg, halls, movies, better) {
		t.Fatal("reported objective does not match the plan")
	}
	if p := Validate(cfg, halls, movies, better); len(p) > 0 {
		t.Fatalf("improved plan violates constraints: %+v", p[:min(3, len(p))])
	}
	if len(better) != len(plan) {
		t.Fatal("improve must not add or drop shows")
	}
	// every film keeps exactly its number of shows and prime shows per day
	count := func(pl []Show) map[string]int {
		m := map[string]int{}
		for _, s := range pl {
			m[fmt.Sprintf("%d|%s|%v", s.MovieID, s.Start.Format("2006-01-02"), cfg.prime(s.Start))]++
		}
		return m
	}
	a, b := count(plan), count(better)
	for k, v := range a {
		if b[k] != v {
			t.Fatalf("show count changed for %s: %d -> %d", k, v, b[k])
		}
	}
}

func TestImprove_PopularFilmMovesToBigHall(t *testing.T) {
	cfg := DefaultConfig(time.Date(2026, 11, 2, 0, 0, 0, 0, trt), 1)
	halls := []Hall{{ID: 1, Seats: 300}, {ID: 2, Seats: 60}}
	movies := []Movie{{ID: 1, DurationMin: 100, Demand: 1}, {ID: 2, DurationMin: 100, Demand: 10}}
	start := cfg.Start.Add(19 * time.Hour)
	end := start.Add(115 * time.Minute)
	plan := []Show{{HallID: 1, MovieID: 1, Start: start, End: end}, {HallID: 2, MovieID: 2, Start: start, End: end}}
	out, st := Improve(cfg, halls, movies, plan)
	if st.Swaps != 1 || out[0].MovieID != 2 || out[1].MovieID != 1 {
		t.Fatalf("popular film should move to the big hall: %+v %+v", out, st)
	}
	// a longer film that would overlap the next show in the big hall stays put
	movies[1].DurationMin = 200
	plan[1].End = start.Add(215 * time.Minute)
	plan = append(plan, Show{HallID: 1, MovieID: 1, Start: end.Add(25 * time.Minute), End: end.Add(140 * time.Minute)})
	_, st = Improve(cfg, halls, movies, plan)
	if st.Swaps != 0 {
		t.Fatalf("swap must respect the next show + cleaning: %+v", st)
	}
}
