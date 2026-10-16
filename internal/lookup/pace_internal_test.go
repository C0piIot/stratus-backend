package lookup

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPacerKeepsRequestsApart is the number MusicBrainz publishes, held
// without sleeping through it: the sleep is recorded rather than taken.
func TestPacerKeepsRequestsApart(t *testing.T) {
	t.Parallel()
	p := newPacer(time.Second)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	var slept []time.Duration
	p.now = func() time.Time { return now }
	p.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		now = now.Add(d)
		return nil
	}

	for range 3 {
		if err := p.wait(t.Context(), "musicbrainz"); err != nil {
			t.Fatal(err)
		}
	}
	// A second service has a clock of its own.
	if err := p.wait(t.Context(), "lrclib"); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{0, time.Second, time.Second, 0}
	if len(slept) != len(want) {
		t.Fatalf("slept %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, slept[i], want[i])
		}
	}
}

func TestPacerOfZeroDoesNotWait(t *testing.T) {
	t.Parallel()
	p := newPacer(0)
	p.sleep = func(context.Context, time.Duration) error { t.Error("slept"); return nil }
	if err := p.wait(t.Context(), "anything"); err != nil {
		t.Fatal(err)
	}
}

func TestSleepForStopsWithTheContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleepFor(ctx, time.Hour); err == nil {
		t.Error("an hour's sleep outlived its context")
	}
}

func TestLuceneQuotesAPhrase(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		`Amber`:            `"Amber"`,
		`Say "Yes"`:        `"Say \"Yes\""`,
		`AC\DC`:            `"AC\\DC"`,
		`Weird AND query:`: `"Weird AND query:"`,
	}
	for in, want := range tests {
		if got := lucene(in); got != want {
			t.Errorf("lucene(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestScoreReadsANumberOrAString(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]score{`100`: 100, `"87"`: 87, `null`: 0, `""`: 0} {
		var s score
		if err := s.UnmarshalJSON([]byte(in)); err != nil || s != want {
			t.Errorf("score %s = %d, %v; want %d", in, s, err, want)
		}
	}
	var s score
	if err := s.UnmarshalJSON([]byte(`"high"`)); err == nil {
		t.Error("a score that is not a number was accepted")
	}
}

func TestIsUUID(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"4cf8ba42-5d0b-3a4d-8e8f-4c8f2f3d1a1b": true,
		"4CF8BA42-5D0B-3A4D-8E8F-4C8F2F3D1A1B": false,
		"not-an-id":                            false,
		"../release/4cf8ba42-5d0b-3a4d-8e8f":   false,
	} {
		if got := isUUID(in); got != want {
			t.Errorf("isUUID(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSleepForWaitsAndIsCutShort(t *testing.T) {
	t.Parallel()
	if err := sleepFor(t.Context(), time.Millisecond); err != nil {
		t.Errorf("a millisecond's sleep = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(5 * time.Millisecond); cancel() }()
	if err := sleepFor(ctx, time.Hour); err == nil {
		t.Error("an hour's sleep outlived a context cancelled under it")
	}
}

func TestFrontCoverNeedsAnID(t *testing.T) {
	t.Parallel()
	s := New(nil, nil, "t")
	if _, err := s.frontCover(t.Context(), "", ""); err == nil {
		t.Error("a cover was fetched for no id at all")
	}
}

func TestCreditNames(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"Autechre":                               {"Autechre"},
		"Architecture In Helsinki, Cameron Bird": {"Architecture In Helsinki, Cameron Bird", "Architecture In Helsinki"},
		"Zero 7 feat. Sia":                       {"Zero 7 feat. Sia", "Zero 7"},
		"Simon & Garfunkel":                      {"Simon & Garfunkel", "Simon"},
		"AC/DC":                                  {"AC/DC"},
		"Crosby, Stills & Nash":                  {"Crosby, Stills & Nash", "Crosby"},
		", Nobody":                               {", Nobody"},
	}
	for in, want := range tests {
		got := creditNames(in)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("creditNames(%q) = %q, want %q", in, got, want)
		}
	}
}
