// Command microvm drives a microvm daemon from the shell.
//
// The command that matters is `run`: point it at a source file and it comes
// back with the output. Everything else is for looking at what the daemon is
// doing.
//
//	microvm run python main.py
//	microvm run node app.ts --network
//	microvm ps
//	microvm logs <sandbox> <exec>
//	microvm queue
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	microvm "github.com/pablofdezr/microvm-sdk-go/microvm"
)

const usage = `microvm — run code in a Firecracker microVM

Usage:
  microvm run <image> <file> [args...]   upload a file, run it, print its output
  microvm exec <image> <cmd> [args...]   run a command in a fresh sandbox
  microvm ps                             list sandboxes on the node
  microvm rm <sandbox-id>                destroy a sandbox
  microvm logs <sandbox-id> <exec-id>    an exec's recorded output
  microvm submit <image> <file>          queue a task, print its ID
  microvm result <task-id>               a task's result
  microvm queue                          queue depth and this node's slots
  microvm images                         images the node can run

Flags:
  -url string      daemon address (default $MICROVM_URL or http://127.0.0.1:8080)
  -token string    bearer token (default $MICROVM_TOKEN)
  -network         give the sandbox filtered internet egress
  -mem int         memory in MiB
  -cpu float       CPU cores, may be fractional
  -timeout dur     kill the process after this long (default 5m)
  -env key=value   set an environment variable; repeatable
  -json            print the raw result as JSON

Exit code mirrors the program's own, so this composes with the shell:
  microvm run python test.py && echo passed
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "microvm:", err)
		os.Exit(1)
	}
}

type options struct {
	url     string
	token   string
	network bool
	mem     int
	cpu     float64
	timeout time.Duration
	env     envFlag
	asJSON  bool
}

// envFlag collects repeated -env key=value flags.
type envFlag map[string]string

func (e envFlag) String() string { return "" }

func (e envFlag) Set(v string) error {
	key, value, found := strings.Cut(v, "=")
	if !found || key == "" {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	e[key] = value
	return nil
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return errors.New("a command is required")
	}

	cmd := os.Args[1]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Print(usage)
		return nil
	}

	opts := options{env: envFlag{}}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.StringVar(&opts.url, "url", envOr("MICROVM_URL", microvm.DefaultBaseURL), "daemon address")
	fs.StringVar(&opts.token, "token", os.Getenv("MICROVM_TOKEN"), "bearer token")
	fs.BoolVar(&opts.network, "network", false, "filtered internet egress")
	fs.IntVar(&opts.mem, "mem", 0, "memory in MiB")
	fs.Float64Var(&opts.cpu, "cpu", 0, "CPU cores")
	fs.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "process timeout")
	fs.Var(opts.env, "env", "environment variable, key=value; repeatable")
	fs.BoolVar(&opts.asJSON, "json", false, "print raw JSON")
	fs.Usage = func() { fmt.Print(usage) }

	// Flags may follow the positional arguments, which is what people actually
	// type: `microvm run python main.py --network`.
	args, flagArgs := splitArgs(os.Args[2:])
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	client := microvm.New(opts.url, microvm.WithToken(opts.token))

	// Ctrl-C aborts the process inside the guest, not just this program: the
	// context reaches the daemon, which kills the whole process group.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "run":
		return cmdRun(ctx, client, opts, args)
	case "exec":
		return cmdExec(ctx, client, opts, args)
	case "ps":
		return cmdPs(ctx, client, opts)
	case "rm":
		return cmdRm(ctx, client, args)
	case "logs":
		return cmdLogs(ctx, client, opts, args)
	case "submit":
		return cmdSubmit(ctx, client, opts, args)
	case "result":
		return cmdResult(ctx, client, opts, args)
	case "queue":
		return cmdQueue(ctx, client)
	case "images":
		return cmdImages(ctx, client)
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// splitArgs separates positional arguments from flags.
//
// Go's flag package stops at the first non-flag, so `run python main.py -network`
// would silently drop -network. Rather than make people type flags first, pull
// them out wherever they are.
func splitArgs(argv []string) (positional, flags []string) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		// A flag that takes a value consumes the next argument, unless it was
		// given as -flag=value.
		if !strings.Contains(a, "=") && takesValue(a) && i+1 < len(argv) {
			i++
			flags = append(flags, argv[i])
		}
	}
	return positional, flags
}

// takesValue reports whether a flag consumes the following argument. Only
// -network and -json are booleans.
func takesValue(flag string) bool {
	switch strings.TrimLeft(flag, "-") {
	case "network", "json":
		return false
	}
	return true
}

// cmdRun uploads a file and runs it with the image's natural interpreter.
func cmdRun(ctx context.Context, client *microvm.Client, opts options, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: microvm run <image> <file> [args...]")
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

	sb, err := newSandbox(ctx, client, image, opts)
	if err != nil {
		return err
	}
	// Its own context: ctx is already cancelled if the user hit Ctrl-C, and a
	// sandbox left running is a VM nobody is watching and everybody is billed
	// for.
	defer client.Sandboxes.Delete(context.WithoutCancel(ctx), sb.Id)

	if _, err := client.Files.Write(ctx, sb.Id, name, source); err != nil {
		return fmt.Errorf("uploading %s: %w", name, err)
	}

	return streamAndExit(ctx, client, sb.Id, cmd, microvm.ExecutionCreateParams{
		Args:           &cmdArgs,
		Env:            envParam(opts.env),
		TimeoutSeconds: seconds(opts.timeout),
	}, opts)
}

// interpreterFor picks how to run a file in a given image.
//
// The mapping is by image rather than by extension: an image is chosen
// deliberately, whereas an extension is a guess about someone's naming.
func interpreterFor(image, file string) (string, []string, error) {
	switch strings.SplitN(image, "-", 2)[0] {
	case "python":
		return "python3", []string{file}, nil
	case "node":
		// tsx runs both .ts and .js, so there is no reason to branch.
		return "tsx", []string{file}, nil
	case "go":
		return "go", []string{"run", file}, nil
	case "rust":
		// rustc has no run mode, so compile and exec in one shell.
		return "sh", []string{"-c",
			fmt.Sprintf("rustc -o /tmp/prog %s && /tmp/prog", file)}, nil
	default:
		return "", nil, fmt.Errorf("no interpreter known for image %q; use `microvm exec` instead", image)
	}
}

func cmdExec(ctx context.Context, client *microvm.Client, opts options, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: microvm exec <image> <cmd> [args...]")
	}

	sb, err := newSandbox(ctx, client, args[0], opts)
	if err != nil {
		return err
	}
	defer client.Sandboxes.Delete(context.WithoutCancel(ctx), sb.Id)

	cmdArgs := args[2:]
	return streamAndExit(ctx, client, sb.Id, args[1], microvm.ExecutionCreateParams{
		Args:           &cmdArgs,
		Env:            envParam(opts.env),
		TimeoutSeconds: seconds(opts.timeout),
	}, opts)
}

func newSandbox(ctx context.Context, client *microvm.Client, image string, opts options) (*microvm.Sandbox, error) {
	params := microvm.SandboxCreateParams{
		Image:   image,
		Network: &opts.network,
		Env:     envParam(opts.env),
		// Outlive the process by a margin, so a run that finishes normally is
		// never racing its own sandbox's TTL.
		TtlSeconds: seconds(opts.timeout + time.Minute),
	}
	if opts.mem > 0 {
		params.MemMib = &opts.mem
	}
	if opts.cpu > 0 {
		params.CpuCores = &opts.cpu
	}

	sb, err := client.Sandboxes.Create(ctx, params)
	if err != nil {
		if microvm.IsCapacity(err) {
			return nil, fmt.Errorf("%w\nthe node is full; `microvm submit` queues the work instead of failing", err)
		}
		return nil, err
	}
	return sb, nil
}

// streamAndExit runs a command, streams its output to this process's stdout and
// stderr, and exits with the program's own status.
//
// Mirroring the exit code is what lets this compose with the shell:
// `microvm run python test.py && deploy` should behave the way it reads.
//
// Starting and watching are two calls now. That is not ceremony: the execution
// belongs to the sandbox rather than to the connection, so a network blip no
// longer kills the run, and the stream replays from the start when you rejoin.
func streamAndExit(ctx context.Context, client *microvm.Client, sandboxID, cmd string, params microvm.ExecutionCreateParams, opts options) error {
	params.Cmd = cmd

	exe, err := client.Executions.Create(ctx, sandboxID, params)
	if err != nil {
		return err
	}

	if opts.asJSON {
		final, err := client.Executions.Wait(ctx, sandboxID, exe.Id)
		if err != nil {
			return err
		}
		if err := printJSON(final); err != nil {
			return err
		}
		return exitWith(final)
	}

	var sawExit bool
	for frame, err := range client.Executions.Stream(ctx, sandboxID, exe.Id) {
		if err != nil {
			return err
		}
		switch frame.Type {
		case microvm.FrameTypeStdout:
			os.Stdout.Write(frame.Bytes())
		case microvm.FrameTypeStderr:
			os.Stderr.Write(frame.Bytes())
		case microvm.FrameTypeExit:
			sawExit = true
		case microvm.FrameTypeError:
			sawExit = true
		}
	}

	// The stream is for showing output; the record is the authority on how it
	// ended. Reading it back also covers the case where Ctrl-C ended the stream
	// while the program was still going.
	final, err := client.Executions.Retrieve(context.WithoutCancel(ctx), sandboxID, exe.Id)
	if err != nil {
		return err
	}
	if !sawExit && !final.Done() {
		return errors.New("the program never reported an exit status")
	}

	switch final.Status {
	case microvm.ExecutionStatusTimedOut:
		fmt.Fprintf(os.Stderr, "\nmicrovm: killed after %v\n", opts.timeout)
	case microvm.ExecutionStatusFailed:
		return fmt.Errorf("could not run %s: %s", cmd, derefStr(final.Error))
	}
	return exitWith(final)
}

// exitWith mirrors the program's exit code into this process's.
func exitWith(exe *microvm.Execution) error {
	if code := exe.ExitCodeOr(0); code != 0 {
		os.Exit(code)
	}
	return nil
}

func cmdPs(ctx context.Context, client *microvm.Client, opts options) error {
	page, err := client.Sandboxes.List(ctx, microvm.SandboxListParams{Limit: 100})
	if err != nil {
		return err
	}
	if opts.asJSON {
		return printJSON(page)
	}
	if len(page.Data) == 0 {
		fmt.Println("no sandboxes")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tIMAGE\tSTATE\tREASON\tAGE\tCPU\tIDLE\tPEAK MEM")
	for _, sb := range page.Data {
		reason := "-"
		if sb.StopReason != nil {
			reason = string(*sb.StopReason)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(sb.Id), sb.Image, sb.State, reason,
			short(time.Since(sb.Created)),
			short(time.Duration(sb.Stats.ActiveCpuMs)*time.Millisecond),
			short(time.Duration(sb.Stats.IdleMs)*time.Millisecond),
			mib(uint64(sb.Stats.MemoryPeakBytes)))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if page.HasMore {
		// Never imply a page is the whole list. A caller who reasons about
		// capacity from a truncated one reasons wrongly.
		fmt.Fprintln(os.Stderr, "\nmicrovm: more sandboxes exist than are shown")
	}
	return nil
}

func cmdRm(ctx context.Context, client *microvm.Client, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: microvm rm <sandbox-id>")
	}
	sb, err := client.Sandboxes.Delete(ctx, args[0])
	if err != nil {
		return err
	}
	// Report the cost. This reply is the only place it exists: the accounting
	// dies with the VM, so a caller who does not read it here never can.
	fmt.Printf("destroyed %s (cpu %s, idle %s, peak %s)\n", sb.Id,
		short(time.Duration(sb.Stats.ActiveCpuMs)*time.Millisecond),
		short(time.Duration(sb.Stats.IdleMs)*time.Millisecond),
		mib(uint64(sb.Stats.MemoryPeakBytes)))
	return nil
}

func cmdLogs(ctx context.Context, client *microvm.Client, opts options, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: microvm logs <sandbox-id> <execution-id>")
	}

	exe, err := client.Executions.Retrieve(ctx, args[0], args[1])
	if err != nil {
		return err
	}
	if opts.asJSON {
		return printJSON(exe)
	}

	os.Stdout.WriteString(exe.Stdout)
	os.Stderr.WriteString(exe.Stderr)

	if deref(exe.StdoutTruncated) || deref(exe.StderrTruncated) {
		fmt.Fprintln(os.Stderr, "\nmicrovm: output was truncated; the oldest lines were dropped")
	}
	// Err covers exactly the endings that are not the code's own verdict, which
	// are the ones worth spelling out.
	if err := exe.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "\n%v\n", err)
	}
	return nil
}

func cmdSubmit(ctx context.Context, client *microvm.Client, opts options, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: microvm submit <image> <file>")
	}
	image, path := args[0], args[1]

	source, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	name := filepath.Base(path)

	cmd, cmdArgs, err := interpreterFor(image, name)
	if err != nil {
		return err
	}

	files := map[string][]byte{name: source}
	params := microvm.TaskCreateParams{
		Image:          image,
		Cmd:            cmd,
		Args:           &cmdArgs,
		Files:          &files,
		Env:            envParam(opts.env),
		TimeoutSeconds: seconds(opts.timeout),
		Network:        &opts.network,
	}
	if opts.mem > 0 {
		params.MemMib = &opts.mem
	}
	if opts.cpu > 0 {
		params.CpuCores = &opts.cpu
	}

	task, err := client.Tasks.Create(ctx, params)
	if err != nil {
		return err
	}
	if opts.asJSON {
		return printJSON(task)
	}
	fmt.Println(task.Id)
	return nil
}

func cmdResult(ctx context.Context, client *microvm.Client, opts options, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: microvm result <task-id>")
	}

	task, err := client.Tasks.Wait(ctx, args[0])
	if err != nil {
		return err
	}
	if opts.asJSON {
		return printJSON(task)
	}

	os.Stdout.WriteString(derefStr(task.Stdout))
	os.Stderr.WriteString(derefStr(task.Stderr))

	if task.Status == microvm.TaskStatusFailed {
		// An infrastructure failure, not the code's verdict. Say which, because
		// a caller must not go debugging code that never ran.
		fmt.Fprintf(os.Stderr, "\nmicrovm: the task could not be run: %s (after %d attempts)\n",
			derefStr(task.Error), deref(task.Attempts))
		os.Exit(1)
	}
	if code := deref(task.ExitCode); code != 0 {
		os.Exit(code)
	}
	return nil
}

func cmdQueue(ctx context.Context, client *microvm.Client) error {
	q, err := client.Queue.Retrieve(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "pending\t%d\n", q.Pending)
	fmt.Fprintf(w, "running\t%d\n", q.Leased)
	fmt.Fprintf(w, "done\t%d\n", q.Done)
	fmt.Fprintf(w, "failed\t%d\n", q.Failed)
	fmt.Fprintf(w, "this node\t%d/%d slots busy\n", q.Busy, q.Slots)
	if q.Pending > 0 {
		// The head's wait is the number that says whether the fleet is big
		// enough; the depth alone does not.
		fmt.Fprintf(w, "oldest wait\t%s\n", short(time.Duration(q.OldestPendingMs)*time.Millisecond))
	}
	return w.Flush()
}

func cmdImages(ctx context.Context, client *microvm.Client) error {
	list, err := client.Images.List(ctx)
	if err != nil {
		return err
	}
	for _, img := range list.Data {
		fmt.Println(img.Id)
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func short(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}

func mib(bytes uint64) string {
	return fmt.Sprintf("%dMB", bytes/(1<<20))
}

// seconds renders a duration as the API's integer seconds, or nil when unset so
// the server applies its own default rather than being told zero.
func seconds(d time.Duration) *int {
	if d <= 0 {
		return nil
	}
	n := int(d.Seconds())
	return &n
}

// envParam passes the env map through, or nil when empty. An empty map and an
// absent one mean the same to the server, and nil is the cheaper of the two.
func envParam(env map[string]string) *map[string]string {
	if len(env) == 0 {
		return nil
	}
	return &env
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func derefStr(p *string) string { return deref(p) }
