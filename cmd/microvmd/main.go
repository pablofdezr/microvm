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

	slots       int
	cpu         float64
	memMiB      int
	warm        string
	snapshotDir string

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

	// Tokens, from three sources that add up. The flags are the old way and keep
	// working; the file and the environment exist because a secret on a command
	// line is a secret in `ps`, in shell history, and in the unit file -- the same
	// argument that keeps a credential flag out of the storage config below.
	tokens          string
	adminTokens     string
	tokensFile      string
	adminTokensFile string

	// Per-tenant admission control. Both are ceilings on one caller, unlike
	// -slots and -ceiling-* which bound the host as a whole: without them any one
	// valid token can hold every slot on the node and call as fast as VMs boot.
	//
	// Charged to a tenant rather than an address, because an address is not an
	// identity. This daemon derives a tenant per token (see principalFor), so here
	// the two coincide and two keys are two allowances -- the per-tenant machinery
	// is what a shared-tenant configuration would use, and there is not one yet.
	tenantMaxSandboxes int
	tenantMaxRPS       float64

	logLevel string

	// How long the daemon remembers what is already over. They are one decision,
	// not two: a stopped sandbox is how an exec record is reached, so the sandbox
	// window is raised to the log window rather than allowed to undercut it.
	logRetention     time.Duration
	sandboxRetention time.Duration

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
	flag.StringVar(&cfg.snapshotDir, "snapshot-dir", "",
		"EXPERIMENTAL: enable Firecracker snapshots (VMs boot with the API socket) and store them here; the warm pool tries to fill by restoring a template snapshot, falling back to cold boot (the restored guest's vsock reconnect is not yet solved)")
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
	flag.StringVar(&cfg.tokens, "tokens", "",
		"DEPRECATED, still honoured: comma-separated bearer tokens. A secret here is a secret in ps, in shell history and in the unit file -- prefer -tokens-file or MICROVM_TOKENS")
	flag.StringVar(&cfg.adminTokens, "admin-tokens", "",
		"DEPRECATED, still honoured: comma-separated bearer tokens with admin power (setting tenant storage policies); a superset of -tokens' abilities. Prefer -admin-tokens-file or MICROVM_ADMIN_TOKENS")
	flag.StringVar(&cfg.tokensFile, "tokens-file", "",
		"file of bearer tokens, one per line (# comments and commas allowed); read at startup and added to MICROVM_TOKENS and -tokens. Keep it root-owned and 0600")
	flag.StringVar(&cfg.adminTokensFile, "admin-tokens-file", "",
		"file of admin bearer tokens, one per line; added to MICROVM_ADMIN_TOKENS and -admin-tokens")
	flag.IntVar(&cfg.tenantMaxSandboxes, "tenant-max-sandboxes", 0,
		"max sandboxes ONE token may have running at once (0 = unlimited). Without it a single token can hold every slot on the node. Does not cover POST /v1/tasks: the count is this node's and a task is scheduled across the fleet, so bound task load with -slots/-cpu/-mem")
	flag.Float64Var(&cfg.tenantMaxRPS, "tenant-max-rps", 0,
		"max API requests per second per token, bursting one second's worth (0 = unlimited). One allowance per tenant, and this daemon derives a tenant per token, so two keys are two allowances")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn or error")
	flag.DurationVar(&cfg.logRetention, "log-retention", time.Hour,
		"how long an exec's output is kept after it finishes")
	flag.DurationVar(&cfg.sandboxRetention, "sandbox-retention", 0,
		"how long a stopped sandbox stays listed and retrievable before the daemon forgets it (0 = forever, which is what every node did before this flag and a slow leak on one that never restarts). Raised to -log-retention when set below it: the stopped record carries the final metering, and every exec record is reached through its sandbox")
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

	// Before anything boots: a bad token path should stop the daemon where an
	// operator is watching, not leave it serving VM creation to strangers.
	tokens, err := loadTokens(cfg, log)
	if err != nil {
		return err
	}
	if len(tokens.tokens) == 0 && len(tokens.admins) == 0 &&
		(cfg.tenantMaxSandboxes > 0 || cfg.tenantMaxRPS > 0) {
		log.Warn("per-tenant limits are set but no tokens are, so there is no identity to charge",
			"hint", "set -tokens-file or MICROVM_TOKENS")
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

		SnapshotDir: cfg.snapshotDir,
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

	if cfg.sandboxRetention > 0 {
		opts = append(opts, sandbox.WithRetention(cfg.sandboxRetention))
	} else {
		// The default cannot change -- forgetting records a node has always kept
		// would change what it answers on upgrade -- but a daemon that never
		// restarts holds every sandbox it has ever run, and serves them all on every
		// list page. An operator should hear that once, at the only moment they are
		// reading.
		log.Warn("stopped sandboxes are never forgotten: the list grows for the daemon's whole life",
			"hint", "set -sandbox-retention (it is raised to -log-retention if lower) to reap them")
	}

	if specs := parseWarmSpecs(cfg.warm, log); len(specs) > 0 {
		if cfg.snapshotDir != "" {
			opts = append(opts, sandbox.WithWarmPoolSnapshots(specs))
		} else {
			opts = append(opts, sandbox.WithWarmPool(specs))
		}
		log.Info("warm pool enabled", "shapes", len(specs), "snapshots", cfg.snapshotDir != "")
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
		if len(tokens.admins) == 0 {
			log.Warn("storage is on but no admin tokens are set: no one can configure tenant limits",
				"hint", "set -admin-tokens-file or MICROVM_ADMIN_TOKENS to grant a key the tenant policy API")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Sweep what is over periodically, or these grow for the daemon's whole life.
	go sweep(ctx, "exec records", cfg.logRetention, logs.Sweep, log)
	if window := mgr.Retention(); window > 0 {
		// The effective window, not the flag: it is raised to -log-retention when it
		// would undercut it, and an operator who asked for five minutes and got an
		// hour should read that here rather than deduce it from a list page.
		if window > cfg.sandboxRetention {
			log.Warn("-sandbox-retention was raised to -log-retention: a stopped sandbox carries the final "+
				"metering, and every exec record is reached through its sandbox",
				"asked", cfg.sandboxRetention, "using", window)
		}
		log.Info("stopped sandboxes are forgotten once their retention window elapses", "retention", window)
		go sweep(ctx, "stopped sandboxes", window, mgr.Sweep, log)
	}

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
		Handler: api.NewServer(apiConfig(cfg, tokens, listImages(cfg.imageDir), tenants),
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

// sweep runs one store's reaper until the daemon stops. Both stores keep records
// past the thing they describe, so both need one.
//
// The interval is a quarter of the retention window and at least a minute, so a
// record outlives its window by a fraction of it rather than by whatever a fixed
// interval happened to be, and a short window never turns this into a busy loop.
func sweep(ctx context.Context, what string, retention time.Duration, reap func() int, log *slog.Logger) {
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
			if n := reap(); n > 0 {
				log.Debug("swept expired records", "what", what, "count", n)
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

// Environment variables the daemon reads tokens from. These are the names
// DEPLOY.md's env file already used -- the unit file expanded them into -tokens,
// which put the secret straight back on the command line the env file existed to
// keep it off. Read directly, they stay in the process's environment.
const (
	envTokens      = "MICROVM_TOKENS"
	envAdminTokens = "MICROVM_ADMIN_TOKENS"
)

// tokenSet is who the daemon accepts, after every source has been read.
type tokenSet struct {
	tokens []string
	admins []string
}

// loadTokens gathers tokens from the file, the environment and the flags.
//
// The sources add up rather than override, which is what makes moving off the
// flags a rotation and not a cutover: add the file, restart, drop the flag,
// restart, and no client is ever refused mid-way.
func loadTokens(cfg config, log *slog.Logger) (tokenSet, error) {
	fromFile, err := readTokenFile(cfg.tokensFile, log)
	if err != nil {
		return tokenSet{}, err
	}
	adminsFromFile, err := readTokenFile(cfg.adminTokensFile, log)
	if err != nil {
		return tokenSet{}, err
	}

	ts := tokenSet{
		tokens: auth.MergeTokens(fromFile,
			auth.ParseTokens(os.Getenv(envTokens)), auth.ParseTokens(cfg.tokens)),
		admins: auth.MergeTokens(adminsFromFile,
			auth.ParseTokens(os.Getenv(envAdminTokens)), auth.ParseTokens(cfg.adminTokens)),
	}

	if cfg.tokens != "" || cfg.adminTokens != "" {
		log.Warn("-tokens/-admin-tokens are deprecated: a secret on the command line is in ps, "+
			"in shell history and in the unit file",
			"hint", "move them to -tokens-file or MICROVM_TOKENS; the flags keep working")
	}
	// Which sources contributed, because "the file I edited is not being read" is
	// the failure this feature invites and one log line settles it.
	log.Info("api tokens loaded",
		"tokens", len(ts.tokens), "admins", len(ts.admins),
		"from_file", len(fromFile)+len(adminsFromFile) > 0,
		"from_env", os.Getenv(envTokens) != "" || os.Getenv(envAdminTokens) != "",
		"from_flags", cfg.tokens != "" || cfg.adminTokens != "")
	return ts, nil
}

// readTokenFile reads one token file, or nothing when no path was given.
//
// A missing or empty file is fatal rather than ignored: an operator who named a
// file meant to require a token, and a daemon that shrugged and started with
// none would be serving VM creation to anyone who found the port.
func readTokenFile(path string, log *slog.Logger) ([]string, error) {
	if path == "" {
		return nil, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read the token file: %w", err)
	}
	// A token file anyone else on the box can read is a shared secret. Warned and
	// not refused: the file is the operator's, and a daemon that will not start
	// over a permission bit is worse than one that says so loudly.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		log.Warn("token file is readable beyond its owner",
			"path", path, "mode", fmt.Sprintf("%04o", mode), "hint", "chmod 600, root-owned")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the token file: %w", err)
	}
	tokens := auth.ParseTokens(string(raw))
	if len(tokens) == 0 {
		return nil, fmt.Errorf("the token file %s holds no tokens", path)
	}
	return tokens, nil
}

// apiConfig assembles the API config, promoting admin tokens to admin
// principals.
//
// When any admin token or per-tenant limit is set, every token has to be spelled
// out as a principal -- there is no longer a single flat list, because admins and
// non-admins are different identities and a limit belongs to one identity rather
// than to the node. Without either, the simple flat list is enough, and that path
// is kept so the common case stays simple.
func apiConfig(cfg config, ts tokenSet, images []string, tenants tenant.Store) api.Config {
	c := api.Config{Images: images, Tenants: tenants}

	if len(ts.admins) == 0 && cfg.tenantMaxSandboxes == 0 && cfg.tenantMaxRPS == 0 {
		c.Tokens = ts.tokens
		return c
	}

	principals := map[string]*auth.Principal{}
	for _, t := range ts.tokens {
		principals[t] = principalFor(cfg, t, false)
	}
	// Admins last, so a token listed as both is an admin rather than the weaker
	// of the two -- the more specific grant wins.
	for _, t := range ts.admins {
		principals[t] = principalFor(cfg, t, true)
	}
	c.Principals = principals
	return c
}

// principalFor builds one token's identity. The limits are the same for every
// token because they come from flags; per-token limits are a configuration
// format this daemon does not have yet, and the Principal is already shaped for
// one when it does.
func principalFor(cfg config, token string, admin bool) *auth.Principal {
	return &auth.Principal{
		Tenant:               auth.DeriveTenant(token),
		Admin:                admin,
		MaxConcurrent:        cfg.tenantMaxSandboxes,
		MaxRequestsPerSecond: cfg.tenantMaxRPS,
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
