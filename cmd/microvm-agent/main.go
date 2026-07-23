// Command microvm-agent is the supervisor that runs inside a guest microVM.
//
// When started as PID 1 it first brings the guest up (mounts, writable overlay,
// networking) and reaps orphaned children for the lifetime of the VM. It then
// serves the control API over AF_VSOCK for the host daemon to drive.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pablofdezr/microvm/internal/agent"
)

func main() {
	var (
		listenMode = flag.String("listen", "vsock", "listener: vsock (in-guest) or tcp (local development)")
		addr       = flag.String("addr", ":5000", "address to bind when -listen=tcp")
		workdir    = flag.String("workdir", "/workspace", "default working directory for execs")
		logLevel   = flag.String("log-level", "info", "debug, info, warn or error")
	)
	flag.Parse()

	log := newLogger(*logLevel)

	if err := run(log, *listenMode, *addr, *workdir); err != nil {
		log.Error("agent exited", "err", err)
		// As PID 1 an exit panics the kernel anyway, but a non-zero status is
		// what the host's supervisor expects when running in a container.
		os.Exit(1)
	}
}

func run(log *slog.Logger, listenMode, addr, workdir string) error {
	// The binary takes one of two roles. As PID 1 the kernel handed us init
	// duties: bring the guest up, then hand serving to a child and spend the
	// rest of our life reaping orphans. RunInit does not return while the guest
	// is healthy.
	//
	// The split exists because PID 1's wait4(-1) would otherwise steal exit
	// statuses from the os/exec calls that run user code; see RunInit.
	if os.Getpid() == 1 && !agent.IsSupervisor() {
		log.Info("running as pid 1, initialising guest")
		if err := agent.InitGuest(log); err != nil {
			return err
		}
		return agent.RunInit(log)
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return err
	}

	ln, err := listen(listenMode, addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler: agent.New(workdir, log).Handler(),
		// No read or write timeout: an exec stream legitimately stays open for
		// as long as the process runs. Per-exec deadlines come from the
		// request's own Timeout field instead.
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		// Outside the guest, Ctrl-C should still shut down cleanly.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("agent listening", "mode", listenMode, "addr", ln.Addr().String(), "version", agent.Version)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func listen(mode, addr string) (net.Listener, error) {
	switch mode {
	case "vsock":
		return agent.ListenVsock()
	case "tcp":
		return net.Listen("tcp", addr)
	default:
		return nil, errors.New("listen must be vsock or tcp")
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	// Log to stderr: inside the guest this lands on the serial console, which
	// is the only way to debug a VM that never got far enough to serve vsock.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
