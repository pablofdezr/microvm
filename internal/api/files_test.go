package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/runtime/runtimetest"
)

// guest returns the fake VM behind a sandbox, which is where a test looks to see
// what actually reached the other side of the vsock.
func (h *harness) guest(t *testing.T, sandboxID string) *runtimetest.Instance {
	t.Helper()
	inst, ok := h.rt.Instance(sandboxID)
	if !ok {
		t.Fatalf("no instance for %s", sandboxID)
	}
	return inst
}

// --- mode -------------------------------------------------------------------

// A mode is only useful if it reaches the guest. It was parsed by the agent and
// dropped by the API for a while, so the assertion that matters is the one on
// the far side, not the one on the reply.
func TestUploadedModeReachesTheGuest(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files",
		map[string]any{"path": "/app/run.sh", "content": []byte("#!/bin/sh\n"), "mode": "0755"})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: %d: %s", resp.StatusCode, raw)
	}

	file := decode[apitypes.File](t, resp)
	if file.Mode == nil || *file.Mode != "0755" {
		t.Errorf("mode = %v, want 0755: the reply states the mode the file ended up with", file.Mode)
	}
	if got, ok := h.guest(t, sb.Id).FileMode("/app/run.sh"); !ok || got != "0755" {
		t.Errorf("the guest was told mode %q (present=%v), want 0755", got, ok)
	}
}

// An upload with no mode is readable and not runnable, and says so.
func TestUploadWithoutAModeIsNotExecutable(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files",
		map[string]any{"path": "/app/main.py", "content": []byte("x = 1\n")})
	file := decode[apitypes.File](t, resp)

	if file.Mode == nil || *file.Mode != defaultFileMode {
		t.Errorf("mode = %v, want %s", file.Mode, defaultFileMode)
	}
	if got, _ := h.guest(t, sb.Id).FileMode("/app/main.py"); got != defaultFileMode {
		t.Errorf("the guest was told mode %q, want %s", got, defaultFileMode)
	}
}

// The host writes uploads as root, so a mode carrying setuid, setgid or the
// sticky bit is refused rather than masked: a root-owned setuid binary left in
// the guest is a root shell for whatever runs there next.
func TestUploadRefusesModesItWillNotHonour(t *testing.T) {
	tests := []struct {
		name string
		mode string
		says string
	}{
		{"setuid", "4755", "setuid"},
		{"setgid", "2755", "setgid"},
		{"sticky", "1777", "sticky"},
		{"not octal", "0678", "not a file mode"},
		{"not a number", "rwx", "not a file mode"},
		{"too few digits", "64", "not a file mode"},
		{"too many digits", "07777", "not a file mode"},
		{"empty", "", "not a file mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			sb := h.createSandbox(t)

			resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files",
				map[string]any{"path": "/app/x", "content": []byte("x"), "mode": tc.mode})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}

			env := decode[apitypes.ErrorEnvelope](t, resp)
			if env.Error.Type != apitypes.ErrorTypeInvalidRequestError {
				t.Errorf("type = %q, want invalid_request_error", env.Error.Type)
			}
			if env.Error.Code != CodeParameterInvalid {
				t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterInvalid)
			}
			if env.Error.Param == nil || *env.Error.Param != "mode" {
				t.Errorf("param = %v, want mode: a caller cannot fix a field we do not name", env.Error.Param)
			}
			if !strings.Contains(env.Error.Message, tc.says) {
				t.Errorf("message = %q, want it to mention %q", env.Error.Message, tc.says)
			}

			if _, ok := h.guest(t, sb.Id).File("/app/x"); ok {
				t.Error("a refused upload still reached the guest")
			}
		})
	}
}

// --- batch ------------------------------------------------------------------

func TestBatchWritesEveryFileInTheOrderGiven(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files/batch", map[string]any{
		"files": []map[string]any{
			{"path": "/app/main.py", "content": []byte("x = 1\n")},
			{"path": "/app/run.sh", "content": []byte("#!/bin/sh\n"), "mode": "755"},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("batch: %d: %s", resp.StatusCode, raw)
	}

	list := decode[apitypes.FileList](t, resp)
	if list.Object != apitypes.FileListObjectList {
		t.Errorf("object = %q, want list", list.Object)
	}
	if list.HasMore {
		t.Error("has_more = true: a batch is not a page")
	}
	if len(list.Data) != 2 {
		t.Fatalf("returned %d files, want 2", len(list.Data))
	}
	if list.Data[0].Path != "/app/main.py" || list.Data[1].Path != "/app/run.sh" {
		t.Errorf("data = %v, want the order the caller gave", list.Data)
	}
	if list.Data[1].Mode == nil || *list.Data[1].Mode != "0755" {
		t.Errorf("mode = %v, want the normalised 0755", list.Data[1].Mode)
	}

	guest := h.guest(t, sb.Id)
	if body, ok := guest.File("/app/main.py"); !ok || string(body) != "x = 1\n" {
		t.Errorf("guest has %q (present=%v)", body, ok)
	}
	if mode, _ := guest.FileMode("/app/run.sh"); mode != "0755" {
		t.Errorf("the guest was told mode %q, want 0755", mode)
	}
}

// The whole point of the batch: one bad entry writes nothing at all. Writing is
// not transactional -- there is no unwriting the third file -- so validation has
// to happen before the first byte leaves, and this is the test that says so.
func TestBatchValidatesEveryEntryBeforeWritingAny(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files/batch", map[string]any{
		"files": []map[string]any{
			{"path": "/app/first.py", "content": []byte("first\n")},
			{"path": "/app/second.py", "content": []byte("second\n"), "mode": "4755"},
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Param == nil || *env.Error.Param != "files[1].mode" {
		t.Errorf("param = %v, want files[1].mode: which entry is the useful half", env.Error.Param)
	}

	if _, ok := h.guest(t, sb.Id).File("/app/first.py"); ok {
		t.Error("the entry before the bad one was written: a rejected batch left a half-written set")
	}
}

func TestBatchMissingPathNamesTheEntry(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files/batch", map[string]any{
		"files": []map[string]any{
			{"path": "/app/first.py", "content": []byte("first\n")},
			{"content": []byte("second\n")},
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeParameterMissing {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterMissing)
	}
	if env.Error.Param == nil || *env.Error.Param != "files[1].path" {
		t.Errorf("param = %v, want files[1].path", env.Error.Param)
	}
}

func TestBatchBounds(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	t.Run("empty", func(t *testing.T) {
		resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files/batch",
			map[string]any{"files": []map[string]any{}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		env := decode[apitypes.ErrorEnvelope](t, resp)
		if env.Error.Code != CodeParameterMissing {
			t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterMissing)
		}
	})

	t.Run("too many", func(t *testing.T) {
		files := make([]map[string]any, 0, maxBatchFiles+1)
		for i := 0; i <= maxBatchFiles; i++ {
			files = append(files, map[string]any{
				"path": fmt.Sprintf("/app/f%d", i), "content": []byte("x"),
			})
		}
		resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files/batch", map[string]any{"files": files})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		env := decode[apitypes.ErrorEnvelope](t, resp)
		if env.Error.Param == nil || *env.Error.Param != "files" {
			t.Errorf("param = %v, want files", env.Error.Param)
		}
		if _, ok := h.guest(t, sb.Id).File("/app/f0"); ok {
			t.Error("an oversized batch wrote its first file anyway")
		}
	})
}

// --- directories ------------------------------------------------------------

func TestCreateDir(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/dirs", map[string]any{"path": "/app/out"})
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("mkdir: %d: %s", resp.StatusCode, raw)
	}

	dir := decode[apitypes.Directory](t, resp)
	if dir.Object != apitypes.DirectoryObjectDirectory {
		t.Errorf("object = %q, want directory: a directory has no bytes, so it is not a file", dir.Object)
	}
	if dir.Path != "/app/out" {
		t.Errorf("path = %q, want /app/out", dir.Path)
	}
	if !h.guest(t, sb.Id).Dir("/app/out") {
		t.Error("no directory reached the guest")
	}

	// mkdir -p semantics: existing is success, so a retry is safe.
	again := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/dirs", map[string]any{"path": "/app/out"})
	if again.StatusCode != http.StatusCreated {
		t.Errorf("status = %d on an existing directory, want 201", again.StatusCode)
	}
}

// A file in the way is the caller confusing two different things, not a repeat
// of the same request, so it is the one case that is not success.
func TestCreateDirOverAFileConflicts(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files",
		map[string]any{"path": "/app/out", "content": []byte("x")})

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/dirs", map[string]any{"path": "/app/out"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeAlreadyExists {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeAlreadyExists)
	}
}

func TestCreateDirRejectsAPathNobodyMeant(t *testing.T) {
	tests := []struct {
		name string
		path string
		code string
	}{
		{"missing", "", CodeParameterMissing},
		{"relative", "app/out", CodeParameterInvalid},
		{"traversal", "/app/../etc", CodeParameterInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			sb := h.createSandbox(t)

			resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/dirs", map[string]any{"path": tc.path})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			env := decode[apitypes.ErrorEnvelope](t, resp)
			if env.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.code)
			}
			if env.Error.Param == nil || *env.Error.Param != "path" {
				t.Errorf("param = %v, want path", env.Error.Param)
			}
		})
	}
}

// A dead sandbox is told apart from a broken one on every route that touches it.
func TestFileRoutesOnAStoppedSandbox(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)
	h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil)

	cases := []struct {
		path string
		body map[string]any
	}{
		{"/files", map[string]any{"path": "/app/x", "content": []byte("x")}},
		{"/files/batch", map[string]any{"files": []map[string]any{{"path": "/app/x", "content": []byte("x")}}}},
		{"/dirs", map[string]any{"path": "/app/out"}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+tc.path, tc.body)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409", resp.StatusCode)
			}
			env := decode[apitypes.ErrorEnvelope](t, resp)
			if env.Error.Code != CodeSandboxNotRunning {
				t.Errorf("code = %q, want %q", env.Error.Code, CodeSandboxNotRunning)
			}
		})
	}
}

// Two entries for one path used to answer 201 reporting two sizes for one file,
// only the later of which was on disk. The reply is meant to be a statement about
// what the sandbox holds, so it is refused rather than silently last-wins -- and
// refused before the first byte goes over, like every other batch rule.
func TestBatchRefusesTheSamePathTwice(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files/batch", map[string]any{
		"files": []map[string]any{
			{"path": "/app/main.py", "content": []byte("first\n")},
			{"path": "/app/other.py", "content": []byte("other\n")},
			{"path": "/app/main.py", "content": []byte("second\n")},
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, raw)
	}

	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeParameterInvalid {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeParameterInvalid)
	}
	if env.Error.Param == nil || *env.Error.Param != "files[2].path" {
		t.Errorf("param = %v, want files[2].path", env.Error.Param)
	}
	// The rule is checked over the whole batch before any of it is written, so the
	// entries before the duplicate must not have landed either.
	if _, ok := h.guest(t, sb.Id).File("/app/other.py"); ok {
		t.Error("a batch refused for a duplicate path wrote its earlier entries anyway")
	}
}

// The route promises the files go in the order given, which is what lets a caller
// whose write failed halfway work out which ones landed. That promise is only
// usable if the failure says where it stopped.
func TestAFailedBatchNamesTheEntryItStoppedAt(t *testing.T) {
	h := newHarness(t)
	sb := h.createSandbox(t)

	// The sandbox goes away between the validation and the writes, which is the only
	// way a batch fails partway once every entry has been checked.
	if resp := h.do(t, "DELETE", "/v1/sandboxes/"+sb.Id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	resp := h.do(t, "POST", "/v1/sandboxes/"+sb.Id+"/files/batch", map[string]any{
		"files": []map[string]any{{"path": "/app/main.py", "content": []byte("x")}},
	})
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, raw)
	}

	env := decode[apitypes.ErrorEnvelope](t, resp)
	if env.Error.Code != CodeSandboxNotRunning {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeSandboxNotRunning)
	}
	if !strings.Contains(env.Error.Message, "files[0]") {
		t.Errorf("message %q does not name the entry it stopped at", env.Error.Message)
	}
}
