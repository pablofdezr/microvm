// Command microvmd is the host daemon.
//
// It owns the host's sandbox capacity: it prepares the network and cgroups,
// serves the public API, and runs a pool of slots that pull queued work.
//
// One daemon per host. Running several would have each install its own firewall
// and fight over the same TAP name space.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pablofdezr/microvm/internal/api"
	"github.com/pablofdezr/microvm/internal/auth"
	"github.com/pablofdezr/microvm/internal/cgroup"
	"github.com/pablofdezr/microvm/internal/logstore"
	"github.com/pablofdezr/microvm/internal/pool"
	"github.com/pablofdezr/microvm/internal/queue"
	fcruntime "github.com/pablofdezr/microvm/internal/runtime/firecracker"
	"github.com/pablofdezr/microvm/internal/sandbox"
	"github.com/pablofdezr/microvm/internal/storage"
	"github.com/pablofdezr/microvm/internal/tenant"
)

type config struct {
	addr       string
	imageDir   string
	kernel     string
	chrootBase string

	slots  int
	cpu    float64
	memMiB int
	warm   string

	redisAddr   string
	redisPrefix string

	slice        string
	ceilingCores float64
	ceilingMemMB int

	poolCIDR string
	uid      int
	gid      int

	netBps   int64
	diskBps  int64
	diskIOPS int64

	tokens      string
	adminTokens string
	logLevel    string

	logRetention time.Duration

	// Storage. There is no credential flag here and there never will be: a
	// secret key passed on a command line is a secret key in `ps`, in the shell
	// history, and in whatever unit file started the daemon. The AWS SDK's own
	// chain -- environment, shared config, instance role -- already solves this,
	// and an instance role never has a value to leak at all.
	s3Bucket       string
	s3Region       string
	s3Endpoint     string
	s3UsePathStyle bool
}

func main() {
	var cfg config

	flag.StringVar(&cfg.addr, "addr", "127.0.0.1:8080",
		"address to serve the API on (loopback by default: this API creates VMs that run arbitrary code)")
	flag.StringVar(&cfg.imageDir, "image-dir", "/var/lib/microvm/images", "directory holding rootfs images")
	flag.StringVar(&cfg.kernel, "kernel", "/var/lib/microvm/vmlinux", "guest kernel")
	flag.StringVar(&cfg.chrootBase, "chroot-base", "/srv/jailer",
		"where jails are built; must share a filesystem with -image-dir so images hardlink instead of copying")
	flag.IntVar(&cfg.slots, "slots", 0, "max concurrent VMs for queued tasks (0 disables the queue worker)")
	flag.Float64Var(&cfg.cpu, "cpu", 0,
		"schedulable CPU cores for queued tasks; tasks are packed so their CPU never exceeds this. 0 means unbounded (pack by -slots and -mem only). Set it on a shared or heterogeneous host")
	flag.StringVar(&cfg.warm, "warm", "",
		"comma-separated warm-pool shapes to pre-boot, image:vcpus:mem:count (e.g. python-arm64.ext4:2:512:2); each shape keeps that many pristine VMs ready to skip the cold boot")
	flag.IntVar(&cfg.memMiB, "mem", 0,
		"schedulable memory in MiB for queued tasks; tasks are packed so their memory never exceeds this. 0 means unbounded. Memory is the dimension that must not oversubscribe, so set it whenever tasks vary in size")
	flag.StringVar(&cfg.redisAddr, "redis", "",
		"Redis address (host:port or redis:// URL) shared by the fleet; empty keeps the queue in this process, which is correct for a single node and wrong for several")
	flag.StringVar(&cfg.redisPrefix, "redis-prefix", "microvm",
		"key namespace in Redis; nodes sharing a prefix share a queue, so this is what separates two fleets on one Redis")
	flag.StringVar(&cfg.slice, "cgroup-slice", "microvm.slice", "cgroup slice holding every sandbox")
	flag.Float64Var(&cfg.ceilingCores, "ceiling-cores", 0,
		"CPU ceiling for ALL sandboxes together (0 = unlimited; set this on a shared host)")
	flag.IntVar(&cfg.ceilingMemMB, "ceiling-mem-mb", 0,
		"memory ceiling in MB for ALL sandboxes together (0 = unlimited)")
	flag.StringVar(&cfg.poolCIDR, "pool-cidr", "172.20.0.0/16", "private network the sandboxes are addressed from")
	flag.IntVar(&cfg.uid, "uid", 0, "unprivileged uid the VMM drops to (required)")
	flag.IntVar(&cfg.gid, "gid", 0, "unprivileged gid the VMM drops to (required)")
	flag.Int64Var(&cfg.netBps, "default-network-bps", 12_500_000,
		"default per-sandbox bandwidth cap in bytes/sec, both ways (0 = unlimited). ~100Mbit by default: nothing else bounds network, and a sandbox on a fraction of a core can still saturate the uplink")
	flag.Int64Var(&cfg.diskBps, "default-disk-bps", 0,
		"default per-sandbox disk bandwidth cap in bytes/sec (0 = unlimited)")
	flag.Int64Var(&cfg.diskIOPS, "default-disk-iops", 0,
		"default per-sandbox disk IOPS cap (0 = unlimited)")
	flag.StringVar(&cfg.s3Bucket, "s3-bucket", "",
		"bucket sandboxes may store files in; empty means sandboxes have no storage")
	flag.StringVar(&cfg.s3Region, "s3-region", "", "bucket region; empty takes it from the environment")
	flag.StringVar(&cfg.s3Endpoint, "s3-endpoint", "",
		"S3 endpoint override, for MinIO, R2 or a test double")
	flag.BoolVar(&cfg.s3UsePathStyle, "s3-path-style", false,
		"address the bucket as endpoint/bucket; required by MinIO and most S3-compatible servers")
	flag.StringVar(&cfg.tokens, "tokens", "", "comma-separated bearer tokens; empty disables auth")
	flag.StringVar(&cfg.adminTokens, "admin-tokens", "",
		"comma-separated bearer tokens with admin power (setting tenant storage policies); a superset of -tokens' abilities")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn or error")
	flag.DurationVar(&cfg.logRetention, "log-retention", time.Hour,
		"how long an exec's output is kept after it finishes")
	flag.Parse()

	log := newLogger(cfg.logLevel)

	if err := run(cfg, log); err != nil {
		log.Error("daemon exited", "err", err)
		os.Exit(1)
	}
}

func run(cfg config, log *slog.Logger) error {
	if os.Geteuid() != 0 {
		// TAP devices, nftables and cgroups all need privilege. Failing here
		// beats failing on the first sandbox with a confusing permission error.
		return errors.New("microvmd must run as root: it manages TAP devices, nftables and cgroups")
	}
	if cfg.uid == 0 || cfg.gid == 0 {
		return errors.New("-uid and -gid are required and must be non-root: they are what the VMM drops to")
	}

	prefix, err := netip.ParsePrefix(cfg.poolCIDR)
	if err != nil {
		return fmt.Errorf("parse -pool-cidr: %w", err)
	}

	ceiling := cgroup.Limits{}
	if cfg.ceilingCores > 0 {
		ceiling.CPU = cgroup.CoresToQuota(cfg.ceilingCores)
	}
	if cfg.ceilingMemMB > 0 {
		ceiling.MemoryMax = uint64(cfg.ceilingMemMB) * 1024 * 1024
	}
	if ceiling.CPU == 0 && ceiling.MemoryMax == 0 {
		// Not fatal -- a dedicated box legitimately wants everything -- but on a
		// host running anything else, sandboxes with no ceiling will eventually
		// starve it.
		log.Warn("no ceiling set: sandboxes may consume the whole host",
			"hint", "set -ceiling-cores and -ceiling-mem-mb if this box runs anything else")
	}

	rt, err := fcruntime.New(fcruntime.Config{
		ChrootBase: cfg.chrootBase,
		ImageDir:   cfg.imageDir,
		KernelPath: cfg.kernel,
		Slice:      cfg.slice,
		Ceiling:    ceiling,
		UID:        cfg.uid,
		GID:        cfg.gid,
		PoolCIDR:   prefix,

		DefaultNetworkBps: cfg.netBps,
		DefaultDiskBps:    cfg.diskBps,
		DefaultDiskIOPS:   cfg.diskIOPS,
	}, log)
	if err != nil {
		return fmt.Errorf("start runtime: %w", err)
	}
	defer rt.Close()

	logs := logstore.New(logstore.Config{Retention: cfg.logRetention})

	var opts []sandbox.Option
	if cfg.s3Bucket != "" {
		// Connecting here rather than lazily on first use is deliberate: bad
		// credentials or a missing bucket should stop the daemon at startup,
		// where an operator is watching, and not surface an hour later as a
		// sandbox's file write mysteriously failing.
		backend, err := storage.NewS3(context.Background(), storage.S3Config{
			Bucket:       cfg.s3Bucket,
			Region:       cfg.s3Region,
			Endpoint:     cfg.s3Endpoint,
			UsePathStyle: cfg.s3UsePathStyle,
		})
		if err != nil {
			return fmt.Errorf("connect storage: %w", err)
		}
		opts = append(opts, sandbox.WithStorage(backend))
		log.Info("sandbox storage enabled", "bucket", cfg.s3Bucket)
	}

	if specs := parseWarmSpecs(cfg.warm, log); len(specs) > 0 {
		opts = append(opts, sandbox.WithWarmPool(specs))
		log.Info("warm pool enabled", "shapes", len(specs))
	}

	mgr := sandbox.NewManager(rt, logs, log, opts...)

	// Tenant policies are only meaningful with a bucket to enforce them against.
	// The store follows the same split as the task queue: in-memory for one node,
	// Redis for a fleet. An admin who sets a limit must have it honoured on every
	// node, so a fleet (any node with -redis) must share this too -- an in-memory
	// store there would let a limit set on one node be invisible on the rest.
	var tenants tenant.Store
	if cfg.s3Bucket != "" {
		if cfg.redisAddr != "" {
			rt, err := tenant.NewRedis(context.Background(), tenant.RedisConfig{
				Addr:   cfg.redisAddr,
				Prefix: cfg.redisPrefix,
			})
			if err != nil {
				return fmt.Errorf("connect to the shared tenant policy store: %w", err)
			}
			defer rt.Close()
			tenants = rt
			log.Info("tenant policies are shared with the fleet", "redis", cfg.redisAddr, "prefix", cfg.redisPrefix)
		} else {
			tenants = tenant.NewMemory()
			log.Info("tenant policies are in-process: a limit set here is not seen by other nodes",
				"hint", "set -redis to share tenant limits across a fleet")
		}
		if cfg.adminTokens == "" {
			log.Warn("storage is on but no -admin-tokens set: no one can configure tenant limits",
				"hint", "set -admin-tokens to grant a key the tenant policy API")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Sweep expired log records periodically, or the store grows for the
	// daemon's whole life.
	go sweepLogs(ctx, logs, cfg.logRetention, log)

	// The queue and the slots are separate decisions, and keeping them separate
	// is what allows a fleet to be shaped rather than cloned. A node with a
	// shared queue and no slots is an API front end; one with slots and no API
	// exposed is a pure worker; one with both is the single-box case. None of
	// them needs to know the others exist.
	var (
		q  queue.Queue
		wp *pool.Pool
	)
	switch {
	case cfg.redisAddr != "":
		q, err = queue.NewRedis(ctx, queue.RedisConfig{
			Addr:   cfg.redisAddr,
			Prefix: cfg.redisPrefix,
		}, log)
		if err != nil {
			return fmt.Errorf("connect to the shared queue: %w", err)
		}
		log.Info("queue is shared with the fleet",
			"redis", cfg.redisAddr, "prefix", cfg.redisPrefix, "slots", cfg.slots)

	case cfg.slots > 0:
		q = queue.NewMemory(queue.MemoryConfig{}, log)
		// Worth saying out loud, because it is invisible until the day it
		// matters: this queue is this process's. A second node would not see
		// these tasks, and a restart drops them.
		log.Info("queue is in-process: tasks are not shared with other nodes and do not survive a restart",
			"hint", "set -redis to share work across a fleet", "slots", cfg.slots)
	}
	if q != nil {
		defer q.Close()
	}

	if cfg.slots > 0 {
		wp, err = pool.New(pool.Config{Slots: cfg.slots, CPU: cfg.cpu, MemMiB: cfg.memMiB}, q, mgr, log)
		if err != nil {
			return fmt.Errorf("create pool: %w", err)
		}
		wp.Start(ctx)
		defer wp.Stop()
	}

	srv := &http.Server{
		Addr: cfg.addr,
		Handler: api.NewServer(apiConfig(cfg, listImages(cfg.imageDir), tenants),
			mgr, q, wp, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a streaming exec legitimately holds a response open
		// for as long as the process runs.
	}

	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("microvmd listening",
		"addr", cfg.addr, "slots", cfg.slots, "images", listImages(cfg.imageDir))

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	// The manager stops every sandbox; the deferred rt.Close then takes the
	// firewall down, in that order -- removing the rules first would leave a
	// live guest with unfiltered egress.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return mgr.Close(shutdownCtx)
}

func sweepLogs(ctx context.Context, logs *logstore.Store, retention time.Duration, log *slog.Logger) {
	interval := retention / 4
	if interval < time.Minute {
		interval = time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := logs.Sweep(); n > 0 {
				log.Debug("swept expired exec records", "count", n)
			}
		}
	}
}

// parseWarmSpecs turns the -warm flag ("image:vcpus:mem:count,...") into warm
// pool shapes. A malformed entry is logged and skipped rather than failing the
// daemon: a typo in a performance knob should not stop it serving. Warm VMs are
// booted without networking, so only network-less tasks of a matching shape are
// served from the pool.
func parseWarmSpecs(s string, log *slog.Logger) []sandbox.WarmSpec {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []sandbox.WarmSpec
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 4 {
			log.Warn("ignoring malformed -warm entry (want image:vcpus:mem:count)", "entry", entry)
			continue
		}
		vcpus, err1 := strconv.Atoi(parts[1])
		mem, err2 := strconv.Atoi(parts[2])
		count, err3 := strconv.Atoi(parts[3])
		if parts[0] == "" || err1 != nil || err2 != nil || err3 != nil || vcpus <= 0 || mem <= 0 || count <= 0 {
			log.Warn("ignoring invalid -warm entry", "entry", entry)
			continue
		}
		out = append(out, sandbox.WarmSpec{Image: parts[0], VCPUs: vcpus, MemMiB: mem, Count: count})
	}
	return out
}

// listImages reports which images the node can actually run, from what is on
// disk rather than from configuration that could disagree with it.
func listImages(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".ext4") {
			continue
		}
		out = append(out, strings.TrimSuffix(filepath.Base(name), ".ext4"))
	}
	return out
}

func splitTokens(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// apiConfig assembles the API config, promoting admin tokens to admin
// principals.
//
// When any admin token is set, every token has to be spelled out as a principal
// -- there is no longer a single flat list, because admins and non-admins are
// different identities. Without admin tokens the simple flat list is enough, and
// that path is kept so the common case stays simple.
func apiConfig(cfg config, images []string, tenants tenant.Store) api.Config {
	c := api.Config{Images: images, Tenants: tenants}

	admins := splitTokens(cfg.adminTokens)
	if len(admins) == 0 {
		c.Tokens = splitTokens(cfg.tokens)
		return c
	}

	principals := map[string]*auth.Principal{}
	for _, t := range splitTokens(cfg.tokens) {
		principals[t] = &auth.Principal{Tenant: auth.DeriveTenant(t)}
	}
	// Admins last, so a token listed as both is an admin rather than the weaker
	// of the two -- the more specific grant wins.
	for _, t := range admins {
		principals[t] = &auth.Principal{Tenant: auth.DeriveTenant(t), Admin: true}
	}
	c.Principals = principals
	return c
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
