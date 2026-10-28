package app

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"
)

// procStat is /proc/stat's first line with the given busy and steal jiffies,
// and a lot of idle that must not count.
func procStat(busy, steal uint64) fstest.MapFS {
	line := fmt.Sprintf("cpu  %d 0 0 999999 50 0 0 %d 0 0\ncpu0 ignored\n", busy, steal)
	return fstest.MapFS{"proc/stat": {Data: []byte(line)}}
}

func TestStealIsTakenAgainstWhatWasWanted(t *testing.T) {
	t.Parallel()
	refused, wanted, ok := steal(procStat(80, 920))
	if !ok || refused != 920 || wanted != 1000 {
		t.Errorf("steal = %d of %d (%v), want 920 of 1000: idle and iowait are not wanted", refused, wanted, ok)
	}
	if _, _, ok := steal(fstest.MapFS{}); ok {
		t.Error("steal said something with no /proc/stat, which is a native Windows or macOS build")
	}
}

func TestThrottledReadsEitherCgroupVersion(t *testing.T) {
	t.Parallel()
	stat := []byte("nr_periods 40\nnr_throttled 30\nthrottled_time 123\n")
	for _, name := range []string{"sys/fs/cgroup/cpu.stat", "sys/fs/cgroup/cpu/cpu.stat", "sys/fs/cgroup/cpu,cpuacct/cpu.stat"} {
		refused, wanted, ok := throttled(fstest.MapFS{name: {Data: stat}})
		if !ok || refused != 30 || wanted != 40 {
			t.Errorf("%s: throttled = %d of %d (%v), want 30 of 40", name, refused, wanted, ok)
		}
	}
	// v2 with the cpu controller off writes the usage and no periods.
	if _, _, ok := throttled(fstest.MapFS{"sys/fs/cgroup/cpu.stat": {Data: []byte("usage_usec 5\n")}}); ok {
		t.Error("a cpu.stat with no periods was read as a quota")
	}
}

// root is a filesystem whose /proc/stat a test moves on.
type root struct{ fs fstest.MapFS }

func (r *root) Open(name string) (fs.File, error) { return r.fs.Open(name) }
func (r *root) at(busy, steal uint64)             { r.fs = procStat(busy, steal) }

func TestTheGaugeWaitsForEnoughToJudgeBy(t *testing.T) {
	t.Parallel()
	r := &root{}
	r.at(0, 0)
	g := newGauge(r, counters)

	if _, _, ok := g.refused(); ok {
		t.Fatal("the first reading said something, with nothing to compare it to")
	}
	// Half a second of work is not enough to judge by, and is kept rather than
	// thrown away: the next reading is taken over both.
	r.at(5, 45)
	if _, _, ok := g.refused(); ok {
		t.Fatal("judged by half a second of work")
	}
	r.at(10, 90)
	share, by, ok := g.refused()
	if !ok || share != 0.9 || by == "" {
		t.Errorf("refused = %v by %q (%v), want 0.9 over the whole second", share, by, ok)
	}
}

// pass drives yielding with a fake clock, recording what it slept.
type pass struct {
	r     *root
	y     *yielding
	slept []time.Duration
	busy  uint64
	steal uint64
}

func newPass() *pass {
	p := &pass{r: &root{}}
	p.r.at(0, 0)
	p.y = newYielding(newGauge(p.r, counters))
	p.y.now = func() time.Time { return time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC) }
	p.y.sleep = func(_ context.Context, d time.Duration) error {
		p.slept = append(p.slept, d)
		return nil
	}
	return p
}

// file is one file processed: wait, then a second of wanted CPU of which
// the given share was refused.
func (p *pass) file(t *testing.T, refused float64) {
	t.Helper()
	if err := p.y.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := uint64(refused * 100)
	p.busy += 100 - s
	p.steal += s
	p.r.at(p.busy, p.steal)
}

func TestAPassBacksOffWhileItIsRefused(t *testing.T) {
	t.Parallel()
	p := newPass()

	p.file(t, 0.03)
	p.file(t, 0.92)
	if len(p.slept) != 0 {
		t.Fatalf("slept %v before anything was refused", p.slept)
	}

	p.file(t, 0.92)
	p.file(t, 0.92)
	p.file(t, 0.03)
	want := []time.Duration{firstPause, 2 * firstPause, 4 * firstPause}
	if fmt.Sprint(p.slept) != fmt.Sprint(want) {
		t.Fatalf("slept %v, want the pause doubling %v", p.slept, want)
	}
	if since, by := p.y.state(); since.IsZero() || by == "" {
		t.Error("standing aside, and the status page was not told")
	}

	// A file given its CPU ends it at once.
	p.file(t, 0.03)
	if len(p.slept) != 3 {
		t.Errorf("slept %v after a file ran at full speed", p.slept)
	}
	if since, _ := p.y.state(); !since.IsZero() {
		t.Error("the status page still says the passes are standing aside")
	}
}

func TestThePauseHasACeiling(t *testing.T) {
	t.Parallel()
	p := newPass()
	p.file(t, 0.92)
	for range 20 {
		p.file(t, 0.92)
	}
	if last := p.slept[len(p.slept)-1]; last != longestPause {
		t.Errorf("the last pause was %v, want it held at %v", last, longestPause)
	}
}

func TestNoSourceMeansNoPause(t *testing.T) {
	t.Parallel()
	y := newYielding(newGauge(fstest.MapFS{}, counters))
	y.sleep = func(context.Context, time.Duration) error {
		t.Fatal("slept on a machine with nothing to read")
		return nil
	}
	for range 3 {
		if err := y.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

// TestASlowSourceKeepsItsWord: a container sees the host's /proc/stat, which
// has enough to judge by long before the container's own quota does, and its
// steal of nothing must not end a pause the quota is causing.
func TestASlowSourceKeepsItsWord(t *testing.T) {
	t.Parallel()
	quota := func(throttles, periods uint64) []byte {
		return fmt.Appendf(nil, "nr_periods %d\nnr_throttled %d\n", periods, throttles)
	}
	r := &root{fs: procStat(0, 0)}
	r.fs["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: quota(0, 0)}
	g := newGauge(r, counters)
	g.refused()

	r.fs = procStat(100, 0)
	r.fs["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: quota(9, 10)}
	if share, _, ok := g.refused(); !ok || share != 0.9 {
		t.Fatalf("refused = %v (%v), want the quota's 0.9", share, ok)
	}
	// The host has another second in it; the container not yet ten periods.
	r.fs = procStat(200, 0)
	r.fs["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: quota(13, 14)}
	if share, by, ok := g.refused(); !ok || share != 0.9 {
		t.Errorf("refused = %v by %q (%v), want the quota's last word to stand", share, by, ok)
	}
}
