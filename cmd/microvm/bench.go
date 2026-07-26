package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	microvm "github.com/pablofdezr/microvm-sdk-go/microvm"
)

// This file exists because of a mistake worth not repeating.
//
// The published benchmark table quoted one number per image -- "python ~0.5s",
// "go ~2.4s" -- and that number was the wall clock of a whole `microvm run`.
// Four unrelated things live inside it: host work, a guest kernel boot, whatever
// the caller's program did (for Go, a compile), and this CLI's own round trips
// to the daemon. Collapsed into a single figure they read as one claim about how
// fast a microVM starts, which is the one thing the figure did not measure. The
// Go row in particular was mostly a compiler.
//
// So the numbers get measured per phase or not at all. Everything below reports
// what it actually timed, on one monotonic clock, and nothing is inferred from
// anything else: `total` is measured directly rather than summed, and the gap
// between it and the phases is printed as `unattributed` instead of being
// quietly distributed into whichever phase would look best.
//
// What this can see is what a client can see. The inside of the boot -- staging,
// the jailer, the guest's kernel and init -- is the daemon's to report, and it
// logs it per create at "sandbox booted"; run the daemon at -log-level=info and
// read it alongside this. The two are deliberately not merged: a client that
// could ask the guest how long it booted would be trusting the guest's word
// about it, and the guest runs code we assume is hostile.

// benchPhase is one span of a single run, in the order they occur.
type benchPhase int

const (
	phaseCreate benchPhase = iota
	phaseUpload
	phaseExecStart
	phaseFirstByte
	phaseProgram
	phaseRetrieve
	phaseDelete
	phaseTotal
	phaseUnattributed
	phaseCount
)

// benchPhaseNames are the column labels, and the words are chosen to resist the
// reading that got the old table wrong. "program" is the caller's own code and
// is not a cost of the sandbox; "create" is the API call and contains the boot
// but is not only the boot.
var benchPhaseNames = [phaseCount]string{
	phaseCreate:       "create",
	phaseUpload:       "upload",
	phaseExecStart:    "exec start",
	phaseFirstByte:    "first byte",
	phaseProgram:      "program",
	phaseRetrieve:     "retrieve",
	phaseDelete:       "delete",
	phaseTotal:        "total",
	phaseUnattributed: "unattributed",
}

// benchPhaseNotes explain, in the output itself, what each span covers. A
// breakdown nobody can interpret is how the aggregate got misread in the first
// place.
var benchPhaseNotes = [phaseCount]string{
	phaseCreate:       "POST /v1/sandboxes: source fetch, boot, seed",
	phaseUpload:       "the file, host -> guest over vsock",
	phaseExecStart:    "POST executions, until the daemon accepts",
	phaseFirstByte:    "exec accepted -> first output frame",
	phaseProgram:      "exec accepted -> exit frame (the caller's own code)",
	phaseRetrieve:     "GET the execution record",
	phaseDelete:       "DELETE the sandbox, synchronous teardown",
	phaseTotal:        "measured, not summed",
	phaseUnattributed: "total minus the phases above",
}

// benchRun is one complete iteration's timings.
type benchRun struct {
	phases [phaseCount]time.Duration
	// exitCode is kept so a run whose program failed is reported rather than
	// averaged in as if it had succeeded. A benchmark of a crashing program
	// measures the crash.
	exitCode int
}

// cmdBench times a full run, phase by phase, over several iterations.
//
// It drives the same public API the CLI does, so what it measures is what a
// caller experiences -- not an internal fast path that no user of the daemon
// ever takes.
func cmdBench(ctx context.Context, client *microvm.Client, opts options, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: microvm bench <image> <file> [args...]")
	}
	image, path, extra := args[0], args[1], args[2:]

	source, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	name := filepath.Base(path)
	cmd, cmdArgs, err := interpreterFor(image, name)
	if err != nil {
		return err
	}
	cmdArgs = append(cmdArgs, extra...)

	if opts.iterations < 1 {
		return fmt.Errorf("-n must be at least 1, got %d", opts.iterations)
	}
	// A negative warmup would make the discarded count exceed the measured count
	// and label the runs backwards, so it is refused rather than clamped: someone
	// who typed it meant something, and guessing which is worse than saying no.
	if opts.warmup < 0 {
		return fmt.Errorf("-warmup cannot be negative, got %d", opts.warmup)
	}

	// A discarded first iteration by default. The very first run of an image on a
	// node pays for reading hundreds of megabytes of rootfs off disk into the host
	// page cache, and that cost is real but is not what a steady-state figure
	// describes -- the published table says "image hot in the page cache", so the
	// harness has to actually make that true rather than assume it. Set
	// -warmup=0 to measure the cold read instead, which is a different and also
	// legitimate question.
	total := opts.warmup + opts.iterations
	runs := make([]benchRun, 0, opts.iterations)

	for i := range total {
		warmup := i < opts.warmup
		label := fmt.Sprintf("run %d/%d", i+1-opts.warmup, opts.iterations)
		if warmup {
			label = fmt.Sprintf("warmup %d/%d", i+1, opts.warmup)
		}
		fmt.Fprintf(os.Stderr, "\r%-24s", label+" ...")

		r, err := benchOnce(ctx, client, opts, image, name, source, cmd, cmdArgs)
		if err != nil {
			fmt.Fprintln(os.Stderr)
			return fmt.Errorf("%s: %w", label, err)
		}
		if !warmup {
			runs = append(runs, r)
		}
	}
	fmt.Fprintf(os.Stderr, "\r%-24s\r", "")

	if opts.asJSON {
		return printJSON(benchJSON(image, name, runs))
	}
	printBenchTable(os.Stdout, image, name, runs)
	return nil
}

// benchOnce performs one create-run-delete cycle, timing each leg.
func benchOnce(ctx context.Context, client *microvm.Client, opts options, image, name string, source []byte, cmd string, cmdArgs []string) (benchRun, error) {
	var r benchRun

	// One clock for the whole iteration. Each phase is closed by reading it
	// again, so the spans abut exactly and nothing falls between two of them
	// except what `unattributed` reports.
	start := time.Now()
	mark := start
	since := func() time.Duration {
		now := time.Now()
		d := now.Sub(mark)
		mark = now
		return d
	}

	sb, err := newSandbox(ctx, client, image, opts)
	if err != nil {
		return r, err
	}
	r.phases[phaseCreate] = since()

	// Deleted even on the failure paths below, and with a context that outlives a
	// Ctrl-C: a benchmark that leaves a VM per iteration behind is a benchmark
	// that changes what it is measuring, and then bills for it.
	deleted := false
	defer func() {
		if !deleted {
			_, _ = client.Sandboxes.Delete(context.WithoutCancel(ctx), sb.Id)
		}
	}()

	if _, err := client.Files.Write(ctx, sb.Id, name, source); err != nil {
		return r, fmt.Errorf("uploading %s: %w", name, err)
	}
	r.phases[phaseUpload] = since()

	exe, err := client.Executions.Create(ctx, sb.Id, microvm.ExecutionCreateParams{
		Cmd:            cmd,
		Args:           &cmdArgs,
		Env:            mapParam(opts.env),
		TimeoutSeconds: seconds(opts.timeout),
	})
	if err != nil {
		return r, err
	}
	r.phases[phaseExecStart] = since()

	// first byte and program are both measured from here, so they overlap rather
	// than abut -- first byte is a point inside program, not a phase before it.
	// That is why program is added to the running clock and first byte is not.
	execAccepted := mark
	var firstByte time.Time
	var sawExit bool

	for frame, err := range client.Executions.Stream(ctx, sb.Id, exe.Id) {
		if err != nil {
			return r, err
		}
		switch frame.Type {
		case microvm.FrameTypeStdout, microvm.FrameTypeStderr:
			// Output is discarded: a benchmark that prints the program's output
			// measures this terminal as much as the sandbox.
			if firstByte.IsZero() {
				firstByte = time.Now()
			}
		case microvm.FrameTypeExit, microvm.FrameTypeError:
			sawExit = true
		}
	}
	r.phases[phaseProgram] = since()
	if !firstByte.IsZero() {
		r.phases[phaseFirstByte] = firstByte.Sub(execAccepted)
	}

	final, err := client.Executions.Retrieve(context.WithoutCancel(ctx), sb.Id, exe.Id)
	if err != nil {
		return r, err
	}
	r.phases[phaseRetrieve] = since()

	if !sawExit && !final.Done() {
		return r, errors.New("the program never reported an exit status")
	}
	if final.ExitCode != nil {
		r.exitCode = *final.ExitCode
	}
	if final.Status == microvm.ExecutionStatusFailed {
		return r, fmt.Errorf("could not run %s: %s", cmd, derefStr(final.Error))
	}

	if _, err := client.Sandboxes.Delete(context.WithoutCancel(ctx), sb.Id); err != nil {
		return r, err
	}
	deleted = true
	r.phases[phaseDelete] = since()

	r.phases[phaseTotal] = time.Since(start)

	// Measured minus accounted-for. It stays visible even when it is small,
	// because the moment it is large this table has stopped explaining the total
	// and the right response is to instrument, not to publish.
	var accounted time.Duration
	for _, p := range []benchPhase{phaseCreate, phaseUpload, phaseExecStart, phaseProgram, phaseRetrieve, phaseDelete} {
		accounted += r.phases[p]
	}
	if r.phases[phaseTotal] > accounted {
		r.phases[phaseUnattributed] = r.phases[phaseTotal] - accounted
	}

	return r, nil
}

// printBenchTable writes the breakdown as markdown, ready to paste into README
// or docs/index.html -- which is the point: a table that has to be retyped is a
// table that drifts from what was measured.
func printBenchTable(w io.Writer, image, file string, runs []benchRun) {
	fmt.Fprintf(w, "image %s, %s, %d run(s)\n\n", image, file, len(runs))

	fmt.Fprintf(w, "| %-13s | %9s | %9s | %9s | %6s | %s\n",
		"phase", "min", "median", "max", "share", "what it covers")
	fmt.Fprintf(w, "|%s|%s|%s|%s|%s|%s\n",
		strings.Repeat("-", 15), strings.Repeat("-", 11), strings.Repeat("-", 11),
		strings.Repeat("-", 11), strings.Repeat("-", 8), strings.Repeat("-", 52))

	medianTotal := median(collect(runs, phaseTotal))

	for p := phaseCreate; p < phaseCount; p++ {
		vals := collect(runs, p)
		med := median(vals)

		// A phase that never happened is left out rather than printed as zero:
		// "first byte" is absent for a program that prints nothing, and a row of
		// zeros invites the reading that it was instant.
		if med == 0 && minOf(vals) == 0 && maxOf(vals) == 0 {
			continue
		}

		share := ""
		// Share of the total is meaningless for the total, and misleading for
		// first byte, which is a point inside program rather than a slice of the
		// whole.
		if p != phaseTotal && p != phaseFirstByte && medianTotal > 0 {
			share = fmt.Sprintf("%5.1f%%", 100*float64(med)/float64(medianTotal))
		}

		fmt.Fprintf(w, "| %-13s | %9s | %9s | %9s | %6s | %s\n",
			benchPhaseNames[p], fmtDur(minOf(vals)), fmtDur(med), fmtDur(maxOf(vals)),
			share, benchPhaseNotes[p])
	}

	fmt.Fprint(w, `
The median is the figure to quote. "program" is the code under test -- for a
compiled language it is mostly the compiler -- and is not a cost of starting a
sandbox. For the inside of "create", read the daemon's own "sandbox booted" line
for these runs: it splits the boot into staging, the jailer, and the guest's
kernel and init.
`)
}

// benchJSON is the -json shape: the raw per-run numbers as well as the summary,
// so a caller can compute a statistic this table does not print rather than
// having to re-run the benchmark to get it.
func benchJSON(image, file string, runs []benchRun) map[string]any {
	phases := make(map[string]any, phaseCount)
	for p := phaseCreate; p < phaseCount; p++ {
		vals := collect(runs, p)
		all := make([]float64, len(vals))
		for i, v := range vals {
			all[i] = msOf(v)
		}
		phases[benchPhaseNames[p]] = map[string]any{
			"min_ms":    msOf(minOf(vals)),
			"median_ms": msOf(median(vals)),
			"max_ms":    msOf(maxOf(vals)),
			"runs_ms":   all,
			"covers":    benchPhaseNotes[p],
		}
	}
	return map[string]any{
		"image":  image,
		"file":   file,
		"runs":   len(runs),
		"phases": phases,
	}
}

func collect(runs []benchRun, p benchPhase) []time.Duration {
	out := make([]time.Duration, len(runs))
	for i, r := range runs {
		out[i] = r.phases[p]
	}
	return out
}

// median sorts a copy: the caller's slice order is the run order, which the JSON
// output reports, and sorting it in place would silently scramble that.
func median(vals []time.Duration) time.Duration {
	if len(vals) == 0 {
		return 0
	}
	s := slices.Clone(vals)
	slices.Sort(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

func minOf(vals []time.Duration) time.Duration {
	if len(vals) == 0 {
		return 0
	}
	return slices.Min(vals)
}

func maxOf(vals []time.Duration) time.Duration {
	if len(vals) == 0 {
		return 0
	}
	return slices.Max(vals)
}

func msOf(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// fmtDur prints a duration at a fixed resolution rather than Go's default,
// which switches units per magnitude and makes a column impossible to scan.
func fmtDur(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	if d < time.Second {
		return fmt.Sprintf("%.1f ms", msOf(d))
	}
	return fmt.Sprintf("%.2f s", d.Seconds())
}
