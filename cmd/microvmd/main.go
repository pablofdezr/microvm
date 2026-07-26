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
	"github.com/pablofdezr/microvm/internal/source"
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

	guestBootVerbose bool

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

	// Seeding a sandbox from a URL the caller names. Off unless -source-fetch is
	// set AND at least one host is allowlisted, because this is the one feature
	// where the daemon makes an outbound request on a caller's behalf -- from
	// outside the firewall it installs for its guests. The allowlist is the
	// control; the caps bound what an allowlisted host can spend.
	sourceFetch            bool
	sourceHosts            repeatedFlag
	sourceCredentials      repeatedFlag
	sourceMaxBytes         int64
	sourceMaxExpandedBytes int64
	sourceMaxFileBytes     int64
	sourceMaxFiles         int
	sourceTimeout          time.Duration
	sourceTempDir          string

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
		"enable Firecracker snapshots (VMs boot with the API socket) and store them here; the warm pool fills by restoring a per-shape template snapshot (a restored guest answers in tens of milliseconds) and falls back to cold boot if any of it fails. Scratch space, not storage: templates are captured per run, discarded at shutdown and swept at startup, so use one directory per daemon and budget one copy of a guest's RAM per warm shape. Networked shapes are never snapshotted. Needs guest images built from this repo at or after the snapshot agent routes, and a guest kernel with /dev/mem on arm64 GICv2 hosts -- see DEPLOY.md")
	flag.BoolVar(&cfg.guestBootVerbose, "guest-boot-verbose", false,
		"leave the guest kernel's boot messages at full verbosity. Off by default because every kernel printk is a synchronous write to an emulated UART that the guest blocks on, and the kernel boot is the largest single phase of a create: quieting it halved that phase on a Pi 5 (169ms of guest kernel to 84ms, and a 288ms create to 170ms). The console stays attached either way, and the threshold only rises to KERN_ERR -- a panic still prints, so does an error that explains a failed boot, and so does everything the guest agent writes. Turn it on when a guest fails in a way the errors alone do not explain")
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
	flag.BoolVar(&cfg.sourceFetch, "source-fetch", false,
		"allow a create to seed a sandbox from a URL the caller names. Off by default, and deliberately: the daemon fetches that URL itself, from outside the firewall it installs for guests, so it is the operator's decision and not something an upgrade turns on. It names no host on its own -- without -source-allow-host every source is still refused. A git source additionally needs the git binary on PATH; a host without git serves tarballs only, and says so at startup")
	flag.Var(&cfg.sourceHosts, "source-allow-host",
		"a host a source may be fetched from; repeatable, and commas in one value are split. An exact name (codeload.github.com) or a leading-dot suffix (.githubusercontent.com), which matches subdomains but not the bare domain. Empty allows nothing, which is what stops -source-fetch alone from reaching every host on the internet. Allowlisting a name does not allowlist an address: where it resolves is checked again at connect time against the private, loopback, link-local and metadata ranges")
	flag.Int64Var(&cfg.sourceMaxBytes, "source-max-bytes", 0,
		"largest compressed source body to download, in bytes (0 = the built-in 64 MiB)")
	flag.Int64Var(&cfg.sourceMaxExpandedBytes, "source-max-expanded-bytes", 0,
		"largest expanded source to write into a sandbox, in bytes (0 = the built-in 256 MiB). This is what bounds a decompression bomb. It is not the sandbox's own limit: a tree that passes here and does not fit the guest's writable layer is refused per sandbox, with both numbers named")
	flag.Int64Var(&cfg.sourceMaxFileBytes, "source-max-file-bytes", 0,
		"largest single file inside a source, in bytes (0 = the built-in 64 MiB). Separate from the total because an archive under the total cap can still hold one member the guest refuses, which fails a write halfway through an expansion that had already validated. Raise it with -source-max-expanded-bytes; the guest itself takes 256 MiB per file")
	flag.IntVar(&cfg.sourceMaxFiles, "source-max-files", 0,
		"most members one source may hold (0 = the built-in 20000). Separate from the byte caps because a 10 KB tar of a million empty files passes both of them. It also bounds what the member names may total, at 256 bytes per member")
	flag.DurationVar(&cfg.sourceTimeout, "source-timeout", 0,
		"how long a fetch or a clone may take (0 = the built-in 60s). It bounds everything a third party controls, and the sandbox is not booted until it is over, so a slow origin costs the caller no VM time")
	flag.StringVar(&cfg.sourceTempDir, "source-temp-dir", "",
		"where a source is buffered or cloned before it is written into the guest (default the system temporary directory). Point it at a real disk if /tmp is a tmpfs on this host: what is staged there is bounded by -source-max-bytes plus -source-max-expanded-bytes per seed, and that would otherwise be memory competing with the VMs")
	flag.Var(&cfg.sourceCredentials, "source-credential",
		"a git credential, as name@https://host/prefix/=/path/to/file; repeatable. The prefix is required and is the only place the credential may be sent: without it any caller could name any credential on any allowlisted URL, which spends the operator's token on repositories the caller was never given and hands the token to whatever host answers. Callers select one by name with credential_ref and never see the value; the file holds a token, or username:token for a forge that wants a real username. A path rather than the secret, for the same reason as -tokens-file: a secret on a command line is a secret in ps, in shell history and in the unit file. Keep it root-owned and 0600")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn or error")
	flag.DurationVar(&cfg.logRetention, "log-retention", time.Hour,
		"how long an exec's output is kept after it finishes")
	flag.DurationVar(&cfg.sandboxRetention, "sandbox-retention", 0,
		"how long a stopped sandbox stays listed and retrievable before the daemon forgets it (0 = forever, which is what every node did before this flag and a slow leak on one that never restarts). Raised to -log-retention when set below it: the stopped record carries the final metering, and every exec record is reached through its sandbox")
	flag.Parse()

	// Refuse a stray argument instead of ignoring it. Go's flag package stops
	// parsing at the first non-flag word, so one typo -- `-source-fetch git`,
	// where -source-fetch is a bool -- silently discards every flag after it.
	// The daemon then starts with a security posture nobody chose: in that exact
	// case an empty -source-allow-host, which the log reports honestly while the
	// operator reads their own command line and sees hosts they named.
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "microvmd: unexpected argument %q; every setting is a flag, and flags must come before it\n", flag.Arg(0))
		flag.Usage()
		os.Exit(2)
	}

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

	// Made here rather than on first use. A missing directory otherwise surfaces
	// as a warm-pool warning thirty seconds in ("snapshot: make dir: no such file
	// or directory"), by which time the pool has cold-booted the shape and the
	// operator has a daemon that looks healthy and quietly does not snapshot.
	// 0700 root-owned: a snapshot is a guest's whole memory, and the VMM reads its
	// copy through the jail rather than from here.
	if cfg.snapshotDir != "" {
		if err := os.MkdirAll(cfg.snapshotDir, 0o700); err != nil {
			return fmt.Errorf("snapshot dir %s: %w", cfg.snapshotDir, err)
		}
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

		SnapshotDir:      cfg.snapshotDir,
		GuestBootVerbose: cfg.guestBootVerbose,
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

	if cfg.sourceFetch {
		// Before anything boots, like the tokens: a credential file that cannot be
		// read should stop the daemon where an operator is watching, not fail the
		// first create that needed a private repository hours later.
		seeder, err := newSeeder(cfg, log)
		if err != nil {
			return err
		}
		opts = append(opts, sandbox.WithSource(seeder))
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

// repeatedFlag is a flag that may be given more than once, keeping every value.
//
// Repeated rather than comma-separated because one of its users is a filesystem
// path, and a path may legitimately contain a comma. -source-allow-host splits on
// commas anyway, since a hostname cannot.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return strings.Join(*r, ",") }

func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// newSeeder builds the source fetcher from the -source-* flags.
//
// Every limit is passed as given, zero included: the package's own defaults are
// deliberately conservative, and a flag nobody set must not become "unbounded".
func newSeeder(cfg config, log *slog.Logger) (*source.Seeder, error) {
	hosts := parseSourceHosts(cfg.sourceHosts)
	creds, err := readCredentials(cfg.sourceCredentials, log)
	if err != nil {
		return nil, err
	}

	seeder, err := source.NewSeeder(source.Config{
		AllowHosts:       hosts,
		MaxBytes:         cfg.sourceMaxBytes,
		MaxExpandedBytes: cfg.sourceMaxExpandedBytes,
		MaxFileBytes:     cfg.sourceMaxFileBytes,
		MaxFiles:         cfg.sourceMaxFiles,
		Timeout:          cfg.sourceTimeout,
		TempDir:          cfg.sourceTempDir,
		Credentials:      creds,
	})
	if err != nil {
		return nil, fmt.Errorf("configure source fetching: %w", err)
	}

	if len(hosts) == 0 {
		// Not fatal: an operator staging the flags should not have the daemon refuse
		// to start. But it does nothing until a host is named, and that is invisible
		// until the first create is refused.
		log.Warn("-source-fetch is set with an empty -source-allow-host, so every source is still refused",
			"hint", "name each host a sandbox may be seeded from, e.g. -source-allow-host codeload.github.com")
	}
	for name, cred := range creds {
		// A credential bound to a host no source may be fetched from can never
		// resolve. Warned rather than refused: an operator may be staging one flag
		// ahead of the other.
		if u, err := source.ParseCredentialPrefix(cred.URLPrefix); err == nil && !hostAllowed(hosts, u.Hostname()) {
			log.Warn("a source credential is bound to a host that is not allowlisted, so it can never be used",
				"credential", name, "host", u.Hostname(),
				"hint", "add -source-allow-host "+u.Hostname())
		}
	}
	if seeder.GitPath() == "" {
		log.Warn("a git source is refused on this host", "reason", seeder.GitNote(),
			"hint", "tarball sources are unaffected")
	}
	sweepSourceLeftovers(cfg.sourceTempDir, log)
	log.Info("source seeding enabled",
		"hosts", len(hosts), "credentials", len(creds), "git", seeder.GitPath())
	return seeder, nil
}

// hostAllowed mirrors the allowlist match in internal/source, for a startup
// warning only. It is not a control: the control is in the package that dials.
func hostAllowed(hosts []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, entry := range hosts {
		entry = strings.ToLower(strings.TrimSuffix(entry, "."))
		if strings.HasPrefix(entry, ".") {
			if strings.HasSuffix(host, entry) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

// sweepSourceLeftovers removes git checkouts a previous run did not finish with.
//
// A clone lives in a named temporary directory until the create that asked for it
// returns, and a SIGKILL in between -- systemd's TimeoutStopSec while a seed is in
// flight is the realistic one -- leaves the whole tree behind with nobody to
// remove it. Swept at startup, like the orphaned TAP devices the runtime clears,
// and for the same reason: nothing else ever will.
//
// Only this daemon's own prefix, and only at startup, so there is no window in
// which it could take a directory a running clone is using.
func sweepSourceLeftovers(tempDir string, log *slog.Logger) {
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	matches, err := filepath.Glob(filepath.Join(tempDir, "microvm-git-*"))
	if err != nil || len(matches) == 0 {
		return
	}
	for _, dir := range matches {
		if err := os.RemoveAll(dir); err != nil {
			log.Warn("could not remove a leftover source checkout", "path", dir, "err", err)
		}
	}
	log.Info("removed leftover source checkouts from a previous run", "count", len(matches))
}

// parseSourceHosts flattens the repeated -source-allow-host into one list. A
// hostname holds no comma, so a value carrying several is split rather than
// refused.
func parseSourceHosts(entries []string) []string {
	var out []string
	for _, entry := range entries {
		for _, host := range strings.Split(entry, ",") {
			if host = strings.TrimSpace(host); host != "" {
				out = append(out, host)
			}
		}
	}
	return out
}

// readCredentials reads the -source-credential files into the map a caller's
// credential_ref resolves against.
//
// The name is the operator's label and is returned on the sandbox; the value never
// leaves this process except into git's environment. Nothing here quotes a value:
// a credential in a startup error is a credential in the journal.
//
// The URL prefix is not optional, and the flag would be a confused deputy without
// it: every caller shares this map, so a credential resolved by name alone is the
// operator's token spent on any repository a caller names and handed to any
// allowlisted host that asks for it. See source.Credential.
func readCredentials(entries []string, log *slog.Logger) (map[string]source.Credential, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	out := make(map[string]source.Credential, len(entries))
	for _, entry := range entries {
		bound, path, ok := strings.Cut(entry, "=")
		name, prefix, scoped := strings.Cut(bound, "@")
		name, prefix, path = strings.TrimSpace(name), strings.TrimSpace(prefix), strings.TrimSpace(path)
		if !ok || !scoped || name == "" || prefix == "" || path == "" {
			return nil, fmt.Errorf(
				"-source-credential %q is not name@https://host/prefix/=/path/to/file", entry)
		}
		if _, err := source.ParseCredentialPrefix(prefix); err != nil {
			return nil, fmt.Errorf("-source-credential %q: %w", name, err)
		}
		if _, dup := out[name]; dup {
			// Refused rather than resolved to the last one: two files for one name is
			// an operator meaning two different things, and guessing which would hand a
			// caller the wrong credential.
			return nil, fmt.Errorf("-source-credential %q is given twice", name)
		}
		secret, err := readSecretFile(path, log)
		if err != nil {
			return nil, fmt.Errorf("read the credential %q: %w", name, err)
		}
		out[name] = source.Credential{URLPrefix: prefix, Secret: secret}
	}
	return out, nil
}

// readSecretFile reads one credential file.
//
// The whole file less its trailing newline, so a token written with `echo` works
// and one with a leading space is not silently corrected. An empty file is fatal
// for the same reason an empty token file is: it means the operator intended a
// secret to be there.
func readSecretFile(path string, log *slog.Logger) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	warnIfShared(path, info.Mode().Perm(), log)

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimRight(string(raw), "\r\n")
	if secret == "" {
		return "", fmt.Errorf("%s holds no credential", path)
	}
	return secret, nil
}

// warnIfShared says so when a file holding a secret is readable beyond its owner.
//
// Warned and not refused: the file is the operator's, and a daemon that will not
// start over a permission bit is worse than one that says so loudly.
func warnIfShared(path string, mode os.FileMode, log *slog.Logger) {
	if mode&0o077 == 0 {
		return
	}
	log.Warn("a file holding a secret is readable beyond its owner",
		"path", path, "mode", fmt.Sprintf("%04o", mode), "hint", "chmod 600, root-owned")
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
	// A token file anyone else on the box can read is a shared secret.
	warnIfShared(path, info.Mode().Perm(), log)

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
