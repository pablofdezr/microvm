package sandbox

import "testing"

// Precedence runs specific-over-general: exec beats sandbox. Getting this
// backwards would silently ignore a caller's per-command override, which is
// the kind of bug that surfaces as "why is it using the wrong token".
func TestMergeEnvPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		sandboxEnv map[string]string
		execEnv    map[string]string
		want       map[string]string
	}{
		{
			name:       "exec overrides sandbox",
			sandboxEnv: map[string]string{"TOKEN": "sandbox", "REGION": "eu"},
			execEnv:    map[string]string{"TOKEN": "exec"},
			want:       map[string]string{"TOKEN": "exec", "REGION": "eu"},
		},
		{
			name:       "sandbox alone",
			sandboxEnv: map[string]string{"API_KEY": "secret"},
			execEnv:    nil,
			want:       map[string]string{"API_KEY": "secret"},
		},
		{
			name:       "exec alone",
			sandboxEnv: nil,
			execEnv:    map[string]string{"ONLY": "exec"},
			want:       map[string]string{"ONLY": "exec"},
		},
		{
			name:       "disjoint sets both survive",
			sandboxEnv: map[string]string{"A": "1"},
			execEnv:    map[string]string{"B": "2"},
			want:       map[string]string{"A": "1", "B": "2"},
		},
		{
			name:       "both empty",
			sandboxEnv: nil,
			execEnv:    nil,
			want:       nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeEnv(tc.sandboxEnv, tc.execEnv)

			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

// The sandbox's map is shared by every exec it will ever run. Merging into it
// would leak one command's variables into the next -- and since these carry
// credentials, that is a cross-request leak, not just a bug.
func TestMergeEnvDoesNotMutateItsInputs(t *testing.T) {
	sandboxEnv := map[string]string{"SHARED": "original"}
	execEnv := map[string]string{"SHARED": "override", "EXTRA": "x"}

	got := mergeEnv(sandboxEnv, execEnv)

	if sandboxEnv["SHARED"] != "original" {
		t.Errorf("the sandbox's env was mutated: SHARED = %q", sandboxEnv["SHARED"])
	}
	if len(sandboxEnv) != 1 {
		t.Errorf("the sandbox's env grew to %v: a later exec would inherit these", sandboxEnv)
	}
	if len(execEnv) != 2 {
		t.Errorf("the exec's env was mutated: %v", execEnv)
	}
	if got["SHARED"] != "override" {
		t.Errorf("merged SHARED = %q, want override", got["SHARED"])
	}

	// Mutating the result must not reach back into the sandbox's map either.
	got["SHARED"] = "mutated-after"
	if sandboxEnv["SHARED"] != "original" {
		t.Error("the result aliases the sandbox's map")
	}
}

// The exec-only path returns the caller's map; make sure the sandbox-only path
// does not hand back the sandbox's own.
func TestMergeEnvSandboxOnlyReturnsACopy(t *testing.T) {
	sandboxEnv := map[string]string{"KEY": "value"}

	got := mergeEnv(sandboxEnv, nil)
	got["KEY"] = "changed"

	if sandboxEnv["KEY"] != "value" {
		t.Error("the sandbox-only path returned the sandbox's own map: a caller can rewrite it")
	}
}
