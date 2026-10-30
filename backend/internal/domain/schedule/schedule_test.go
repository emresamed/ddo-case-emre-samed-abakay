package schedule

import (
	"testing"
	"time"
)

var trt = time.FixedZone("TRT", 3*3600)

func fixture() (Config, []Hall, []Movie) {
	cfg := DefaultConfig(time.Date(2026, 11, 2, 0, 0, 0, 0, trt), 14)
	var halls []Hall
	for i := 1; i <= 10; i++ {
		halls = append(halls, Hall{ID: int64(i), Name: "Salon"})
	}
	movies := []Movie{
		{1, "Kuzey Yıldızı", 128, 30, 2500},
		{2, "Son Vapur", 104, 25, 2000},
		{3, "Bozkırda Bir Gece", 142, 20, 2500},
		{4, "Ada Çocukları (Animasyon)", 95, 30, 1000},
		{5, "Kayıp Defter", 117, 20, 2000},
		{6, "Hamlet (Sahne Kaydı)", 165, 8, 3000},
		{7, "Kırmızı Çizgi", 110, 25, 2500},
		{8, "Sessiz Liman", 99, 15, 1500},
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
		if !(s.MovieID == 1 && cfg.week(s.Start) == 0) {
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
		{HallID: 1, MovieID: 1, Start: early, End: early.Add(143 * time.Minute)},             // before opening
		{HallID: 1, MovieID: 99, Start: early, End: early},                                     // unknown movie
		{HallID: 99, MovieID: 1, Start: early, End: early},                                     // unknown hall
		{HallID: 2, MovieID: 1, Start: cfg.Start.Add(12 * time.Hour), End: cfg.Start.Add(13 * time.Hour)}, // wrong length
		{HallID: 3, MovieID: 1, Start: cfg.Start.AddDate(0, 0, -1), End: cfg.Start},           // out of range
	}
	p := Validate(cfg, halls, movies, bad)
	for _, code := range []string{"OUTSIDE_HOURS", "UNKNOWN_MOVIE", "UNKNOWN_HALL", "BAD_LENGTH", "OUT_OF_RANGE"} {
		if !has(p, code) {
			t.Errorf("missing %s in %+v", code, p)
		}
	}
}
