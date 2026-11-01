package ratelimit

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestEvictOldestIsNotQuadratic pins the cost of the eviction path.
//
// The original implementation kept an n-element "oldest" slice and, for every
// remaining map entry, linearly scanned that slice to find its newest member.
// With maxKeys=200000 and evictBatch=20000 that is ~180,000 x 20,000 = 3.6
// BILLION comparisons, all under the global mutex. Measured at 6.45s of
// lock-held single-threaded work.
//
// Its comment justified the design as avoiding "a full sort of a 200k-entry
// map". That sort is ~200k*log2(200k) = ~3.6M operations: a thousand times
// cheaper than the loop written to avoid it. The optimisation cost three orders
// of magnitude more than the thing it optimised away.
//
// This matters because the eviction runs on the same hot path as the sweep that
// was rate-limited earlier: an unauthenticated client controls the ingest
// limiter's key, so it can hold the map at the ceiling and make every request
// pay. Rate-limiting the 2.76ms sweep while leaving a 6.45s eviction in place
// moved the wedge threshold without removing it.
//
// HOW this is asserted matters as much as what.
//
// The first version of this test measured one eviction and compared it to a
// fixed 500ms budget. That is a statement about the machine, not about the
// algorithm, and it went red on 2026-07-30 while two mirror publishes were
// building on the same runner. It passed on an idle one. Worse than the noise:
// ci-go stops at the first failing directory, so this flake sat in front of a
// real data race in hephaestus and a broken test in sentinel and hid both for
// as long as it had been failing.
//
// So assert the SHAPE of the cost instead of its magnitude, by measuring two
// sizes on the same machine in the same run and comparing them.
//
// The first attempt at that DOUBLED n and required the ratio to stay under 3.0,
// reasoning that quadratic work grows ~4x per doubling and the sort-based
// implementation ~2.1x, so 3.0 sits in the gap. It went red anyway on 2026-08-08
// at 3.03x, while publish-mesh-mirror built on the same runner.
//
// The threshold was not the problem; the step size was. Measured on an idle
// laptop, the real eviction's own ratio wandered between 1.32x and 3.57x per
// doubling. This is a Go map of 200k entries: the cost is memory latency, and it
// moves with cache state and allocator behaviour. So the measurement noise was
// about as large as the whole 2.1x-to-4x window the test was trying to resolve,
// and no threshold placed inside that window can be reliable.
//
// QUADRUPLE n instead. The separation grows much faster than the noise does:
//
//	               2x step        4x step
//	n log n        ~2.1x          ~4.5x
//	quadratic      ~4x            ~16x
//
// And then do NOT hardcode what that ratio should be, because it is not a
// property of the algorithm. It is a property of the machine, and this test has
// now been wrong about that three times.
//
// The measured ratio moves with the CPU, and it moves with instrumentation. On
// an arm64 laptop under -race the real eviction reports 8.65x-19.31x per
// quadrupling; on the amd64 CI runner, under the same -race flag and the same
// image, it reports 4.86x. Same code, same build flags, roughly four times the
// answer. A threshold calibrated on either machine is wrong on the other, and a
// threshold calibrated on a developer laptop is wrong in CI, which is the only
// place it runs.
//
// So calibrate IN the run. Measure a deliberately quadratic workload alongside
// the real one, on the same machine, in the same conditions, and compare the two
// measurements rather than either measurement against a constant. Whatever the
// machine does to the real eviction it does to the reference too, so the
// comparison survives what the absolute numbers cannot.
//
// A healthy eviction is far cheaper-growing than the reference. A regressed one
// converges on it: if evictOldest went back to the O(n*batch) scan, its ratio
// and the reference's would be the same number, and the separation collapses to
// ~1.0. The smallest separation observed on healthy code, across both machines,
// both build modes, idle and under an 8-way CPU load, was 2.40x.
//
// Wall time still includes descheduling. In the 2026-11-01 filtered publication,
// the healthy sort-based implementation measured 9.84x against a 16.67x
// reference and failed the unchanged 2x separation. Each sample now runs in a
// separate copy of this test binary and uses its completed process user+system
// CPU time. Startup, map/slice construction, GC and teardown are INCLUDED; no
// baseline is subtracted. Four repetitions amortize fixed process startup.
// This changes the clock, not the algorithm, sizes, functional checks or bars.
// The quadratic reference must still resolve at least 8x growth, and a quadratic
// candidate must still fail the same 2x-separation decision.
func TestEvictOldestIsNotQuadratic(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a 200k-entry map")
	}

	realRatio, small, large := quietestEvictRatio(t, maxKeys/4, maxKeys)
	t.Logf("eviction process CPU including setup at n=%d: %s; at n=%d: %s (growth %.2fx)",
		maxKeys/4, small, maxKeys, large, realRatio)

	// A measurement too small to resolve makes the ratio meaningless: at
	// microsecond scale the timer granularity dominates and the test would
	// report noise as a complexity regression.
	if small < 100*time.Microsecond {
		t.Skipf("baseline %s is below timer resolution for a meaningful ratio", small)
	}

	// The reference, measured here and now, on the same 4x step.
	//
	// Sized 12500 -> 50000 rather than smaller. A quarter of this costs a quarter
	// of the time and was tried first, but at 6250 -> 25000 the reference itself
	// gets noisy in an uninstrumented build (measured 10.97x-14.64x against a
	// theoretical 16x), and a noisy DENOMINATOR is the one thing the separation
	// check cannot tolerate: it failed a healthy tree at 1.79x. The reference has
	// to be the steady half of the comparison.
	const refSmallN, refLargeN = 12500, 50000
	reference, refSmall, refLarge := quietestRatio(t, "quadratic", refSmallN, refLargeN)
	t.Logf("quadratic process CPU including setup at n=%d: %s; at n=%d: %s (growth %.2fx)",
		refSmallN, refSmall, refLargeN, refLarge, reference)

	// If the reference does not itself look quadratic, nothing on this machine is
	// resolving growth and the comparison below means nothing. Theory says 16x and
	// the lowest ever measured was 15.89x, so 8x is a broken-harness floor rather
	// than a calibration.
	const referenceFloor = 8.0
	if reference < referenceFloor {
		t.Fatalf("the quadratic reference measured only %.2fx per quadrupling (%s -> %s), "+
			"under the %.1fx floor. The harness is not resolving growth on this machine, "+
			"so it cannot judge the real eviction either. Fix the measurement before "+
			"trusting a pass here.", reference, refSmall, refLarge, referenceFloor)
	}

	// A quadratic regression makes these two numbers converge.
	const minSeparation = 2.0
	if realRatio*minSeparation > reference {
		t.Errorf("eviction grew %.2fx per quadrupling (%s -> %s) against %.2fx for a "+
			"deliberately quadratic reference measured in the same run: only %.2fx apart, "+
			"want at least %.1fx. Eviction runs under the single global mutex on a path an "+
			"unauthenticated caller controls, so this cost is lock contention every other "+
			"request pays for.",
			realRatio, small, large, reference, reference/realRatio, minSeparation)
	}

	// It must still actually evict, and evict the RIGHT ones: the entries
	// closest to expiry are the least useful to keep.
	l := filledLimiter(maxKeys)
	l.evictOldest(evictBatch, time.Now())
	if got := len(l.hits); got > maxKeys-evictBatch {
		t.Errorf("evicted too few: %d entries remain, want <= %d", got, maxKeys-evictBatch)
	}
	// The earliest reset (k0) should be gone; a late one should survive.
	if _, ok := l.hits["k0"]; ok {
		t.Error("k0 has the earliest reset and should have been evicted first")
	}
	if _, ok := l.hits["k"+strconv.Itoa(maxKeys-1)]; !ok {
		t.Error("the entry with the latest reset was evicted; eviction is picking the wrong end")
	}
}

// filledLimiter builds a limiter holding n entries whose reset times ascend with
// the key index, so "oldest" is unambiguous and k0 is always the first to go.
func filledLimiter(n int) *Limiter {
	l := New(100, time.Minute)
	now := time.Now()
	for i := 0; i < n; i++ {
		l.hits["k"+strconv.Itoa(i)] = &entry{count: 1, reset: now.Add(time.Duration(i) * time.Millisecond)}
	}
	return l
}

// measureEvict performs the unchanged eviction workload. The parent accounts
// for the WHOLE child process, including filledLimiter and runtime.GC, rather
// than trusting its returned wall interval. The unchanged workload returns a
// duration for historical callers; the CPU sampler deliberately ignores it.
func measureEvict(n int) time.Duration {
	l := filledLimiter(n)
	now := time.Now()

	runtime.GC()
	prevGC := debug.SetGCPercent(-1)
	start := time.Now()
	l.evictOldest(n/10, now)
	d := time.Since(start)
	debug.SetGCPercent(prevGC)

	return d
}

// quietestEvictRatio reduces each size to its fastest observed eviction and
// returns large/small. See quietestRatio for why it is built the way it is; the
// proof that it still refuses a genuinely quadratic implementation is the
// reference measurement inside TestEvictOldestIsNotQuadratic, which runs the
// same estimator over a workload that IS quadratic.
func quietestEvictRatio(t *testing.T, nSmall, nLarge int) (ratio float64, small, large time.Duration) {
	return quietestRatio(t, "evict", nSmall, nLarge)
}

// quietestRatio retains the five interleaved independent minima. Each duration
// is process CPU including setup; unrelated processes and time descheduled are
// not charged to this child. A helper error or unavailable CPU accounting fails
// closed instead of becoming a skipped or zero-duration sample.
func quietestRatio(t *testing.T, kind string, nSmall, nLarge int) (ratio float64, small, large time.Duration) {
	t.Helper()
	measure := func(n int) time.Duration { return evictionProcessCPU(t, kind, n) }
	const rounds = 5
	for i := 0; i < rounds; i++ {
		// INTERLEAVED, so both sizes are sampled across the same stretch of wall
		// clock. Measuring every small to completion first and only then the larges
		// lets a busy window overlapping just the second half inflate the ratio, and
		// that is a bias rather than noise: it can only push toward failure.
		if s := measure(nSmall); small == 0 || s < small {
			small = s
		}
		if l := measure(nLarge); large == 0 || l < large {
			large = l
		}
	}
	// INDEPENDENT minima, not the best single pair. Taking min(large/small) over
	// pairs looks equivalent and is not: one round whose SMALL sample was stalled
	// yields a small ratio from a large denominator, and the minimum then selects
	// exactly that round. Measured under an 8-way CPU load, the paired form
	// reported 1.98x for a workload that is genuinely ~16x, because one small
	// sample took 64ms instead of 11ms. Reducing each side on its own discards that
	// outlier instead of preferring it.
	return float64(large) / float64(small), small, large
}

// quadraticEvict reproduces the SHAPE of the implementation the guard refuses:
// for every entry to evict, linearly scan all remaining entries to find the
// oldest. That is O(batch*n), so doubling n quadruples the work.
//
// It is a stand-in rather than the original code because the original ran at
// maxKeys=200000 and took 6.45s; the same shape at a fraction of that size makes
// the point in milliseconds. Quiesced identically to measureEvict so the two are
// comparable.
func quadraticEvict(n int) time.Duration {
	vals := make([]int64, n)
	for i := range vals {
		vals[i] = int64(n - i)
	}
	batch := n / 10

	runtime.GC()
	prevGC := debug.SetGCPercent(-1)
	start := time.Now()
	for b := 0; b < batch; b++ {
		oldest, idx := int64(math.MaxInt64), -1
		for i, v := range vals {
			if v >= 0 && v < oldest {
				oldest, idx = v, i
			}
		}
		if idx >= 0 {
			vals[idx] = -1
		}
	}
	d := time.Since(start)
	debug.SetGCPercent(prevGC)

	return d
}

// A fixed batch reduces startup's contribution without dropping it from the
// numerator or denominator. Samples are sequential, with the inherited CI
// environment and resource limits; no subprocess creates another subprocess.
const evictionCPURepetitions = 4

func evictionProcessCPU(t *testing.T, kind string, n int) time.Duration {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate eviction CPU test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestEvictionCPUHelper$", "-test.count=1")
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), "FLARE_EVICTION_CPU_WORK="+kind+":"+strconv.Itoa(n))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("eviction CPU helper %s/%d failed: %v (stdout=%d bytes, stderr=%d bytes)", kind, n, err, stdout.Len(), stderr.Len())
	}
	want := fmt.Sprintf("FLARE_EVICTION_CPU_DONE %s %d %d", kind, n, evictionCPURepetitions)
	ack := 0
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == want {
			ack++
		} else if line != "PASS" {
			t.Fatalf("unexpected eviction CPU helper output (%d bytes)", stdout.Len())
		}
	}
	if ack != 1 || stderr.Len() != 0 || cmd.ProcessState == nil || !cmd.ProcessState.Success() {
		t.Fatal("eviction CPU helper did not acknowledge exactly one complete workload")
	}
	cpu := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	if cpu <= 0 {
		t.Fatal("eviction CPU accounting is unavailable")
	}
	return cpu / evictionCPURepetitions
}

// Only the exact self-executable run above supplies this private workload
// marker. The normal package suite runs the parent assertion instead.
func TestEvictionCPUHelper(t *testing.T) {
	work := os.Getenv("FLARE_EVICTION_CPU_WORK")
	if work == "" {
		return
	}
	kind, size, ok := strings.Cut(work, ":")
	n, err := strconv.Atoi(size)
	if !ok || err != nil || (n != 12500 && n != maxKeys/4 && n != maxKeys) {
		t.Fatal("invalid eviction CPU workload size")
	}
	for i := 0; i < evictionCPURepetitions; i++ {
		switch kind {
		case "evict":
			measureEvict(n)
		case "quadratic":
			quadraticEvict(n)
		default:
			t.Fatal("invalid eviction CPU workload kind")
		}
	}
	fmt.Printf("FLARE_EVICTION_CPU_DONE %s %d %d\n", kind, n, evictionCPURepetitions)
}
