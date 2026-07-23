//go:build linux

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pablofdezr/microvm/internal/protocol"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

// arch names the image variant to test against. The images are per-architecture
// because the runtimes inside them are native binaries.
func arch() string {
	if a := os.Getenv("MICROVM_TEST_ARCH"); a != "" {
		return a
	}
	return "arm64"
}

// langCase is one language's proof that it actually runs.
type langCase struct {
	// image is the rootfs filename, minus the arch suffix.
	image string

	// file is written into the sandbox, and cmd/args run it. Together they are
	// the real question: can a caller send source and get output back?
	file    string
	source  string
	cmd     string
	args    []string
	want    string
	timeout time.Duration

	// memMiB overrides the default. Compilers need considerably more than
	// interpreters, and too little shows up as an OOM kill rather than as a
	// compiler error.
	memMiB int
}

func languageCases() []langCase {
	return []langCase{
		{
			image:   "python",
			file:    "main.py",
			source:  "import sys\nprint('python', sys.version_info.major, sys.version_info.minor)\n",
			cmd:     "python3",
			args:    []string{"main.py"},
			want:    "python 3",
			timeout: 30 * time.Second,
			memMiB:  256,
		},
		{
			image: "node",
			file:  "main.ts",
			// TypeScript, not JavaScript, and deliberately: a caller sending a
			// .ts file must not have to compile it first. tsx is in the image
			// so this needs no network.
			source:  "const greet = (n: string): string => `typescript ${n}`;\nconsole.log(greet(process.version));\n",
			cmd:     "tsx",
			args:    []string{"main.ts"},
			want:    "typescript v22",
			timeout: 60 * time.Second,
			memMiB:  512,
		},
		{
			image:   "go",
			file:    "main.go",
			source:  "package main\n\nimport (\n\t\"fmt\"\n\t\"runtime\"\n)\n\nfunc main() {\n\tfmt.Println(\"go\", runtime.Version())\n}\n",
			cmd:     "go",
			args:    []string{"run", "main.go"},
			want:    "go go1.",
			timeout: 120 * time.Second,
			// Compiling needs real memory; 256MB OOMs partway through the
			// compiler and looks like a mysterious kill.
			memMiB: 640,
		},
		{
			image:   "rust",
			file:    "main.rs",
			source:  "fn main() {\n    println!(\"rust works\");\n}\n",
			cmd:     "sh",
			args:    []string{"-c", "rustc -o /tmp/prog main.rs && /tmp/prog"},
			want:    "rust works",
			timeout: 120 * time.Second,
			memMiB:  640,
		},
	}
}

// TestLanguagesRunRealCode is the product test: for each language, upload a
// source file, run it, get its output. Everything else in the system exists to
// make this work.
func TestLanguagesRunRealCode(t *testing.T) {
	for _, tc := range languageCases() {
		t.Run(tc.image, func(t *testing.T) {
			image := tc.image + "-" + arch() + ".ext4"
			requireImage(t, image)

			mgr := newTestManager(t)
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout+90*time.Second)
			defer cancel()

			sb, err := mgr.Create(ctx, sandbox.Spec{
				Spec: runtime.Spec{
					ID:     "lang-" + tc.image,
					Image:  image,
					VCPUs:  2,
					MemMiB: tc.memMiB,
					Limits: runtime.Limits{CPUCores: 1, DiskMiB: 256},
				},
				TTL:         5 * time.Minute,
				IdleTimeout: -1,
			})
			if err != nil {
				t.Fatalf("create %s sandbox: %v", tc.image, err)
			}
			defer sb.Stop(context.Background(), sandbox.ReasonStopped)

			if err := sb.WriteFile(ctx, tc.file, strings.NewReader(tc.source), ""); err != nil {
				t.Fatalf("upload %s: %v", tc.file, err)
			}

			start := time.Now()
			err = sb.Exec(ctx, protocol.ExecRequest{
				ID: "run-" + tc.image, Cmd: tc.cmd, Args: tc.args, Timeout: tc.timeout,
			}, nil)
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			elapsed := time.Since(start)

			rec, ok := sb.Logs("run-" + tc.image)
			if !ok {
				t.Fatal("no log record")
			}

			if rec.ExitCode == nil || *rec.ExitCode != 0 {
				t.Fatalf("exit code = %s (status %s)\nstdout: %s\nstderr: %s",
					formatExitCode(rec.ExitCode), rec.Status, rec.Stdout, rec.Stderr)
			}
			if !strings.Contains(string(rec.Stdout), tc.want) {
				t.Errorf("stdout = %q, want it to contain %q\nstderr: %s",
					rec.Stdout, tc.want, rec.Stderr)
			}

			info := sb.Info()
			t.Logf("%s ran in %v (active CPU %v, peak %dMB): %s",
				tc.image, elapsed.Round(time.Millisecond),
				info.Stats.ActiveCPU.Round(time.Millisecond),
				info.Stats.MemoryPeak/(1024*1024),
				strings.TrimSpace(string(rec.Stdout)))
		})
	}
}

// TestSandboxInstallsDependencies proves the filtered egress is actually usable:
// blocking the private ranges must not have broken the package managers, which
// is the one network thing sandboxes genuinely need.
func TestSandboxInstallsDependencies(t *testing.T) {
	image := "python-" + arch() + ".ext4"
	requireImage(t, image)

	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	sb, err := mgr.Create(ctx, sandbox.Spec{
		Spec: runtime.Spec{
			ID: "lang-pipinstall", Image: image, VCPUs: 2, MemMiB: 512,
			Network: true, // the point of the test
			Limits:  runtime.Limits{CPUCores: 1, DiskMiB: 256},
		},
		TTL:         5 * time.Minute,
		IdleTimeout: -1,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer sb.Stop(context.Background(), sandbox.ReasonStopped)

	err = sb.Exec(ctx, protocol.ExecRequest{
		ID:  "pip",
		Cmd: "sh",
		// A tiny package with no dependencies: this tests the network path, not
		// pip's ability to build wheels.
		Args:    []string{"-c", "pip install --no-cache-dir --quiet six && python3 -c 'import six; print(\"installed six\", six.__version__)'"},
		Timeout: 120 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	rec, _ := sb.Logs("pip")
	if rec.ExitCode == nil || *rec.ExitCode != 0 {
		t.Fatalf("pip install failed with %v\nstdout: %s\nstderr: %s",
			rec.ExitCode, rec.Stdout, rec.Stderr)
	}
	if !strings.Contains(string(rec.Stdout), "installed six") {
		t.Errorf("stdout = %q", rec.Stdout)
	}
	t.Logf("filtered egress still allows package installs: %s", strings.TrimSpace(string(rec.Stdout)))
}

// formatExitCode renders a *int exit code readably. Passing the pointer
// straight to %v prints its address, which looks like a wild exit status and
// sends you hunting for a bug that is in the error message.
func formatExitCode(code *int) string {
	if code == nil {
		return "none (the process never reported an exit)"
	}
	return strconv.Itoa(*code)
}

func requireImage(t *testing.T, name string) {
	t.Helper()
	_, rootfs := requireEnv(t)
	path := filepath.Join(filepath.Dir(rootfs), name)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("image %s not present: %v", name, err)
	}
}
