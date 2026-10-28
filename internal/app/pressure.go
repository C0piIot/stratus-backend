package app

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/web"
)

// The background passes stand aside while the machine cannot give them the
// CPU they ask for (#455).
//
// **What is measured is how much of the CPU that was wanted was not given**,
// and not how busy the machine is: a pass on a one-CPU machine keeps it busy
// by itself, and that is the pass working rather than the machine being
// short. Two sources say it, both as a pair of counters that only move while
// something is runnable -- so a pass that has stopped does not read its own
// silence as the machine recovering:
//
//   - the CPU a hypervisor took back, `steal` in /proc/stat, against
//     everything the guest ran or tried to run. This is how a VM with a
//     shared CPU is throttled, Fly's among them: on the demo, out of burst
//     balance, it read 90-93% while Fly reported the same number throttled,
//     and 2-3% at full speed the minute before.
//   - the periods a cgroup's CPU quota cut short, against the periods it ran
//     in -- Docker's --cpus and a Kubernetes limit, v2 or v1.
//
// **PSI was measured and left out.** Throughout that same throttling it read
// under 5%: with one worker nothing in the guest is waiting for the CPU,
// because the vCPU itself is not running. It sees tasks contending with each
// other and not a machine being slowed down. Nor is Go's scheduler latency
// here, which is blind the same way and was the one signal a native Windows
// or macOS build would have had: there, and anywhere neither file exists,
// there is no reading and the passes run as they always have.
//
// **And it backs off rather than waits for the signal to clear**, because the
// signal is only there while the pass runs: paused, nothing is runnable and
// both counters stand still. So a file is processed, what it was given is
// read, and a pass that is being refused sleeps a little longer each time --
// one file per pause -- until a file is processed at full speed again.

const (
	// refusedAbove is the share of wanted CPU not given past which a pass
	// stands aside. The two measured states were 2-3% and 90-93%, so the
	// number in between matters less than that it is between them.
	refusedAbove = 0.5
	// firstPause and longestPause bound the back-off. A pause doubles while a
	// pass keeps being refused, so a machine that stays starved still gets a
	// file processed every five minutes rather than none.
	firstPause   = 5 * time.Second
	longestPause = 5 * time.Minute
)

// counter is one source: something that was not given, out of what was
// wanted, both cumulative. ok is false where the source does not exist.
type counter struct {
	name string
	read func(fs.FS) (refused, wanted uint64, ok bool)
	// enough is the least wanted between two readings for their ratio to mean
	// anything: a second of CPU in jiffies, ten quota periods.
	enough uint64
}

var counters = []counter{
	{name: "CPU taken back by the hypervisor", read: steal, enough: 100},
	{name: "CPU quota of the container", read: throttled, enough: 10},
}

// gauge reads the counters and keeps the reading each ratio is taken against,
// and the last ratio each one gave.
type gauge struct {
	root  fs.FS
	from  []counter
	last  map[string][2]uint64
	share map[string]float64
}

func newGauge(root fs.FS, from []counter) *gauge {
	return &gauge{root: root, from: from, last: map[string][2]uint64{}, share: map[string]float64{}}
}

// refused is the largest share of wanted CPU not given, and which source said
// it. ok is false when no source has seen enough wanted since it last said
// anything -- which leaves the reading to accumulate rather than be taken over
// a few milliseconds of work.
//
// A source that has nothing new keeps its last word. Otherwise one that fills
// its quota of readings sooner speaks for both: a container's /proc/stat is
// the whole host's, which has a second of CPU in it long before the
// container's cgroup has ten periods, and its steal of nothing would end a
// pause the quota was still causing.
func (g *gauge) refused() (share float64, by string, ok bool) {
	for _, c := range g.from {
		r, w, exists := c.read(g.root)
		if !exists {
			continue
		}
		prev, seen := g.last[c.name]
		if !seen || w < prev[1] || r < prev[0] {
			g.last[c.name] = [2]uint64{r, w}
			continue
		}
		dw := w - prev[1]
		if dw < c.enough {
			continue
		}
		g.last[c.name] = [2]uint64{r, w}
		g.share[c.name] = float64(r-prev[0]) / float64(dw)
		ok = true
	}
	if !ok {
		return 0, "", false
	}
	for _, c := range g.from {
		if s, known := g.share[c.name]; known && (by == "" || s > share) {
			share, by = s, c.name
		}
	}
	return share, by, true
}

// yielding is the back-off the passes go through, and what /status says
// about it.
type yielding struct {
	gauge *gauge
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu    sync.Mutex
	pause time.Duration
	since time.Time
	by    string
}

func newYielding(g *gauge) *yielding {
	return &yielding{gauge: g, now: time.Now, sleep: sleepCtx}
}

// wait is called by a pass before each file. It returns at once unless the
// last stretch of work was refused its CPU, and then after a pause that
// doubles while that keeps being true.
func (y *yielding) wait(ctx context.Context) error {
	share, by, ok := y.gauge.refused()

	y.mu.Lock()
	switch {
	case ok && share > refusedAbove:
		if y.pause == 0 {
			y.since = y.now()
		}
		y.pause = min(max(2*y.pause, firstPause), longestPause)
		y.by = by
	case ok:
		y.pause, y.since, y.by = 0, time.Time{}, ""
	}
	// With no reading the state stands: a pass that was paused processes one
	// file per pause until there is enough to judge by.
	pause := y.pause
	y.mu.Unlock()

	if pause == 0 {
		return ctx.Err()
	}
	return y.sleep(ctx, pause)
}

// state is what /status shows: since when the passes have been standing
// aside and why, or a zero time when they are not.
func (y *yielding) state() (since time.Time, by string) {
	y.mu.Lock()
	defer y.mu.Unlock()
	return y.since, y.by
}

// yieldingNow is the same for the web UI's status page.
func (a *App) yieldingNow() web.Yielding {
	since, by := a.yielding.state()
	return web.Yielding{Since: since, By: by}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// steal reads the aggregate cpu line of /proc/stat: steal, against user,
// nice, system, irq, softirq and steal -- what ran or was runnable. Idle and
// iowait are left out, which is what keeps a paused pass from diluting it.
func steal(root fs.FS) (refused, wanted uint64, ok bool) {
	b, err := fs.ReadFile(root, "proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(line))
	// cpu user nice system idle iowait irq softirq steal ...
	if len(f) < 9 || f[0] != "cpu" {
		return 0, 0, false
	}
	var v [9]uint64
	for i := 1; i < 9; i++ {
		n, perr := strconv.ParseUint(f[i], 10, 64)
		if perr != nil {
			return 0, 0, false
		}
		v[i] = n
	}
	return v[8], v[1] + v[2] + v[3] + v[6] + v[7] + v[8], true
}

// throttled reads nr_throttled against nr_periods from the cgroup's cpu.stat:
// where v2 keeps it, or where v1 mounts the cpu controller under either of its
// two names. A cgroup with no quota counts no periods and so never has enough
// to say anything, which is right.
func throttled(root fs.FS) (refused, wanted uint64, ok bool) {
	for _, name := range []string{
		"sys/fs/cgroup/cpu.stat",
		"sys/fs/cgroup/cpu/cpu.stat",
		"sys/fs/cgroup/cpu,cpuacct/cpu.stat",
	} {
		b, err := fs.ReadFile(root, name)
		if err != nil {
			continue
		}
		var periods, throttles uint64
		var hasP, hasT bool
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), " ")
			n, perr := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if perr != nil {
				continue
			}
			switch k {
			case "nr_periods":
				periods, hasP = n, true
			case "nr_throttled":
				throttles, hasT = n, true
			}
		}
		if hasP && hasT {
			return throttles, periods, true
		}
	}
	return 0, 0, false
}
