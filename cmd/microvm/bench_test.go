package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"
)

func ms(n float64) time.Duration {
	return time.Duration(n * float64(time.Millisecond))
}

func TestMedian(t *testing.T) {
	tests := []struct {
		name string
		in   []time.Duration
		want time.Duration
	}{
		{"empty is zero", nil, 0},
		{"single value", []time.Duration{ms(7)}, ms(7)},
		{"odd count takes the middle", []time.Duration{ms(30), ms(10), ms(20)}, ms(20)},
		{"even count averages the two middles", []time.Duration{ms(10), ms(20), ms(30), ms(40)}, ms(25)},
		{"unsorted input", []time.Duration{ms(100), ms(1), ms(50)}, ms(50)},
		{"all equal", []time.Duration{ms(5), ms(5), ms(5)}, ms(5)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := median(tc.in); got != tc.want {
				t.Fatalf("median(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The JSON output reports per-run timings in run order, so a median that sorted
// its argument in place would scramble the record of which run was which -- and
// would do it silently, after the numbers had already been collected.
func TestMedianDoesNotMutateItsInput(t *testing.T) {
	in := []time.Duration{ms(30), ms(10), ms(20)}
	original := slices.Clone(in)

	median(in)

	if !slices.Equal(in, original) {
		t.Fatalf("median mutated its input: got %v, want %v", in, original)
	}
}

func TestMinMaxOf(t *testing.T) {
	vals := []time.Duration{ms(30), ms(10), ms(20)}

	if got := minOf(vals); got != ms(10) {
		t.Errorf("minOf = %v, want %v", got, ms(10))
	}
	if got := maxOf(vals); got != ms(30) {
		t.Errorf("maxOf = %v, want %v", got, ms(30))
	}

	// Empty must be zero rather than a panic: a phase that never fired has no
	// values, and the table skips it by reading exactly these zeros.
	if got := minOf(nil); got != 0 {
		t.Errorf("minOf(nil) = %v, want 0", got)
	}
	if got := maxOf(nil); got != 0 {
		t.Errorf("maxOf(nil) = %v, want 0", got)
	}
}

func TestFmtDur(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		// Zero is a phase that did not happen, and printing "0.0 ms" would claim
		// it happened instantly.
		{0, "-"},
		{ms(0.5), "0.5 ms"},
		{ms(164), "164.0 ms"},
		{ms(999.9), "999.9 ms"},
		{time.Second, "1.00 s"},
		{ms(2400), "2.40 s"},
		{27 * time.Second, "27.00 s"},
	}

	for _, tc := range tests {
		if got := fmtDur(tc.in); got != tc.want {
			t.Errorf("fmtDur(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMsOf(t *testing.T) {
	// Microsecond resolution has to survive the conversion: the whole reason the
	// guest reports microseconds is that millisecond rounding erases the phases
	// being split.
	if got := msOf(1500 * time.Microsecond); got != 1.5 {
		t.Fatalf("msOf(1500us) = %v, want 1.5", got)
	}
	if got := msOf(0); got != 0 {
		t.Fatalf("msOf(0) = %v, want 0", got)
	}
}

func TestCollect(t *testing.T) {
	runs := []benchRun{
		{phases: [phaseCount]time.Duration{phaseCreate: ms(100), phaseProgram: ms(10)}},
		{phases: [phaseCount]time.Duration{phaseCreate: ms(200), phaseProgram: ms(20)}},
	}

	got := collect(runs, phaseCreate)
	want := []time.Duration{ms(100), ms(200)}
	if !slices.Equal(got, want) {
		t.Fatalf("collect(create) = %v, want %v", got, want)
	}

	// Run order is preserved, because that is what the JSON output claims to
	// report.
	if got := collect(runs, phaseProgram); !slices.Equal(got, []time.Duration{ms(10), ms(20)}) {
		t.Fatalf("collect(program) = %v, want run order", got)
	}
}

// Every phase needs a label and a note, or the table prints a blank column
// header for a real measurement and the reader is left guessing -- which is the
// failure the whole breakdown exists to fix.
func TestEveryPhaseIsDescribed(t *testing.T) {
	for p := phaseCreate; p < phaseCount; p++ {
		if benchPhaseNames[p] == "" {
			t.Errorf("phase %d has no name", p)
		}
		if benchPhaseNotes[p] == "" {
			t.Errorf("phase %q has no note explaining what it covers", benchPhaseNames[p])
		}
	}
}

// A plausible python run: a fast boot, a trivial program, and a synchronous
// teardown on the caller's clock. The shape of this table is the deliverable, so
// it is worth asserting rather than eyeballing once.
func benchFixture() []benchRun {
	mk := func(create, upload, execStart, firstByte, program, retrieve, del, total time.Duration) benchRun {
		return benchRun{phases: [phaseCount]time.Duration{
			phaseCreate: create, phaseUpload: upload, phaseExecStart: execStart,
			phaseFirstByte: firstByte, phaseProgram: program, phaseRetrieve: retrieve,
			phaseDelete: del, phaseTotal: total,
		}}
	}
	return []benchRun{
		mk(ms(164), ms(12), ms(8), ms(30), ms(45), ms(4), ms(60), ms(295)),
		mk(ms(170), ms(11), ms(9), ms(32), ms(47), ms(5), ms(62), ms(306)),
		mk(ms(160), ms(13), ms(7), ms(29), ms(44), ms(4), ms(58), ms(288)),
	}
}

func TestPrintBenchTable(t *testing.T) {
	var buf bytes.Buffer
	printBenchTable(&buf, "python", "hello.py", benchFixture())
	out := buf.String()

	t.Run("names the image, the file and the run count", func(t *testing.T) {
		if !strings.Contains(out, "image python, hello.py, 3 run(s)") {
			t.Errorf("header missing from:\n%s", out)
		}
	})

	t.Run("prints a row per measured phase", func(t *testing.T) {
		for _, want := range []string{"create", "upload", "exec start", "first byte", "program", "retrieve", "delete", "total"} {
			if !strings.Contains(out, want) {
				t.Errorf("no row for %q", want)
			}
		}
	})

	t.Run("prints the median rather than the mean", func(t *testing.T) {
		// Medians of the fixture: create 164, delete 60, total 295.
		for _, want := range []string{"164.0 ms", "60.0 ms", "295.0 ms"} {
			if !strings.Contains(out, want) {
				t.Errorf("expected median %q in:\n%s", want, out)
			}
		}
	})

	t.Run("gives the total no share of itself", func(t *testing.T) {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "| total ") && strings.Contains(line, "%") {
				t.Errorf("total row carries a share: %q", line)
			}
		}
	})

	// first byte is a point inside program, not a slice of the total, so giving it
	// a percentage would invite adding it to the other shares.
	t.Run("gives first byte no share of the total", func(t *testing.T) {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "| first byte ") && strings.Contains(line, "%") {
				t.Errorf("first byte row carries a share: %q", line)
			}
		}
	})

	t.Run("says the program is the code under test, not a sandbox cost", func(t *testing.T) {
		if !strings.Contains(out, "the caller's own code") {
			t.Error("the program row does not disclaim being a sandbox cost")
		}
	})

	// The whole reason this harness exists: the reader must be pointed at where
	// the inside of the boot is reported, rather than left to assume create is
	// all boot.
	t.Run("points the reader at the daemon's own boot breakdown", func(t *testing.T) {
		if !strings.Contains(out, "sandbox booted") {
			t.Error("the footer does not mention the daemon's boot log line")
		}
	})
}

// A phase that never fired is omitted, because a row of dashes-or-zeros reads as
// "instant" rather than "did not happen".
func TestPrintBenchTableOmitsPhasesThatNeverFired(t *testing.T) {
	runs := []benchRun{{phases: [phaseCount]time.Duration{
		phaseCreate: ms(100), phaseTotal: ms(100),
	}}}

	var buf bytes.Buffer
	printBenchTable(&buf, "python", "quiet.py", runs)
	out := buf.String()

	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "| first byte ") {
			t.Errorf("first byte was printed for a program that produced none: %q", line)
		}
		if strings.HasPrefix(line, "| upload ") {
			t.Errorf("upload was printed though it never fired: %q", line)
		}
	}
	if !strings.Contains(out, "create") {
		t.Error("create should still be printed")
	}
}

func TestBenchJSONReportsEveryPhaseAndTheRawRuns(t *testing.T) {
	runs := []benchRun{
		{phases: [phaseCount]time.Duration{phaseCreate: ms(100), phaseTotal: ms(150)}},
		{phases: [phaseCount]time.Duration{phaseCreate: ms(200), phaseTotal: ms(250)}},
	}

	out := benchJSON("python", "hello.py", runs)

	if out["runs"] != 2 {
		t.Errorf("runs = %v, want 2", out["runs"])
	}

	phases, ok := out["phases"].(map[string]any)
	if !ok {
		t.Fatalf("phases is %T, want map[string]any", out["phases"])
	}
	for p := phaseCreate; p < phaseCount; p++ {
		if _, ok := phases[benchPhaseNames[p]]; !ok {
			t.Errorf("phases is missing %q", benchPhaseNames[p])
		}
	}

	create, ok := phases["create"].(map[string]any)
	if !ok {
		t.Fatalf("create is %T, want map[string]any", phases["create"])
	}
	if create["median_ms"] != 150.0 {
		t.Errorf("create median_ms = %v, want 150", create["median_ms"])
	}
	if create["min_ms"] != 100.0 {
		t.Errorf("create min_ms = %v, want 100", create["min_ms"])
	}
	if create["max_ms"] != 200.0 {
		t.Errorf("create max_ms = %v, want 200", create["max_ms"])
	}

	// The raw per-run numbers are the point of -json: a caller wanting a
	// statistic this table does not print must not have to re-run the benchmark.
	raw, ok := create["runs_ms"].([]float64)
	if !ok {
		t.Fatalf("create runs_ms is %T, want []float64", create["runs_ms"])
	}
	if !slices.Equal(raw, []float64{100, 200}) {
		t.Errorf("create runs_ms = %v, want [100 200]", raw)
	}
}
