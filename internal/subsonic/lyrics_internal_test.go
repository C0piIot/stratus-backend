package subsonic

import "testing"

// TestParseLRC is the file format LRCLIB keeps synced words in: a time in
// front of each line, several in front of a line sung more than once, and tag
// lines that are not words at all.
func TestParseLRC(t *testing.T) {
	t.Parallel()
	text := "[ar: Björk]\n[00:05.00][01:10.00] chorus\n[00:01.50] first\n\n[00:03.123] second\r\n[bad] not a time\n[unterminated\n[00:02] bare seconds"
	got := parseLRC(text)

	want := []struct {
		start int64
		value string
	}{
		{1500, "first"}, {2000, "bare seconds"}, {3123, "second"}, {5000, "chorus"}, {70000, "chorus"},
	}
	if len(got) != len(want) {
		t.Fatalf("parseLRC gave %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if *got[i].Start != w.start || got[i].Value != w.value {
			t.Errorf("line %d = %d %q, want %d %q", i, *got[i].Start, got[i].Value, w.start, w.value)
		}
	}
}

func TestLRCTime(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{
		"00:00.00": 0, "01:02.03": 62030, "01:02.034": 62034, "01:02": 62000, "10:59.9": 659900, "00:01.123456": 1123,
	} {
		got, ok := lrcTime(in)
		if !ok || got != want {
			t.Errorf("lrcTime(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "ar: x", "1:60.00", "-1:00.00", "a:b", "00:0x", "00:01.ab", "00:01.-5"} {
		if _, ok := lrcTime(in); ok {
			t.Errorf("lrcTime(%q) was accepted", in)
		}
	}
}
