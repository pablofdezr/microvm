package api

import (
	"context"
	"net/http"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/id"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/sandbox"
	"github.com/pablofdezr/microvm/internal/storage"
)

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	var params apitypes.SandboxCreateParams
	if err := decodeBody(w, r, &params); err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	if params.Image == "" {
		s.writeAPIError(w, r, missingParamError("image"))
		return
	}

	spec := sandbox.Spec{
		Spec: runtime.Spec{
			// Server-generated, always. A caller-chosen ID would let one tenant
			// collide with another's sandbox, or probe for it.
			ID:    id.New(id.SandboxPrefix),
			Image: params.Image,
			Limits: runtime.Limits{
				CPUCores:   deref(params.CpuCores),
				DiskMiB:    deref(params.DiskMib),
				NetworkBps: int64(deref(params.NetworkBps)),
				DiskBps:    int64(deref(params.DiskBps)),
				DiskIOPS:   int64(deref(params.DiskIops)),
			},
			VCPUs:   deref(params.Vcpus),
			MemMiB:  deref(params.MemMib),
			Network: deref(params.Network),
		},
		TTL: time.Duration(deref(params.TtlSeconds)) * time.Second,
	}
	if params.Env != nil {
		spec.Env = *params.Env
	}
	// Idle needs care that TTL does not: zero means "use the default" and
	// negative means "never reclaim", so an absent field and an explicit 0 are
	// the same thing while an explicit -1 is not.
	if params.IdleSeconds != nil {
		spec.IdleTimeout = time.Duration(*params.IdleSeconds) * time.Second
	}

	// The storage mount comes from who the caller is, never from what they sent.
	// The prefix names a tenant, and a tenant a body could choose is a tenant a
	// body could steal; so the only thing the request is allowed to say about
	// storage is read_only, and even that can only tighten what the token grants.
	if p := principalFrom(r.Context()); p != nil {
		readOnly := p.ReadOnly
		if params.Storage != nil && params.Storage.ReadOnly != nil {
			readOnly = readOnly || *params.Storage.ReadOnly
		}
		mount := &storage.Mount{
			Prefix:   sandbox.TenantPrefix(p.Tenant),
			Quota:    p.Quota,
			ReadOnly: readOnly,
		}
		// The mount path is the one storage field the body may freely choose: it
		// names a directory in the guest's own namespace, not a tenant, so it
		// cannot reach another caller's data. It is validated all the same,
		// because it ends up on the kernel command line where a malformed value
		// would inject a boot parameter rather than a path.
		if params.Storage != nil && params.Storage.MountPath != nil {
			if !storage.ValidMountPath(*params.Storage.MountPath) {
				s.writeAPIError(w, r, invalidParamError("storage.mount_path",
					"must be an absolute, clean path using only letters, digits, and _-./"))
				return
			}
			mount.MountPath = *params.Storage.MountPath
		}
		// The tenant's cap and eviction policy come from the policy store, set by
		// an admin -- never from this request. A caller cannot raise their own
		// limit, which is the whole reason the policy lives on the server side.
		if s.cfg.Tenants != nil {
			if policy, ok, err := s.cfg.Tenants.Get(r.Context(), p.Tenant); err == nil && ok {
				mount.Tenant = policy
			}
		}
		spec.Storage = mount
	}

	sb, err := s.mgr.Create(r.Context(), spec)
	if err != nil {
		// A failed create is nearly always the node being full. It is reported
		// as capacity rather than an internal error because the two call for
		// different reactions: back off and retry, or stop and read the logs.
		s.log.Warn("create sandbox failed", "image", params.Image, "err", err)
		s.writeAPIError(w, r, capacityError(err))
		return
	}

	writeJSON(w, http.StatusCreated, toAPISandbox(sb))
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	page, err := parsePageParams(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	all := s.mgr.List()

	if want := r.URL.Query().Get("state"); want != "" {
		filtered := all[:0:0]
		for _, sb := range all {
			if string(sb.Info().State) == want {
				filtered = append(filtered, sb)
			}
		}
		all = filtered
	}

	items, hasMore := paginate(all, func(sb *sandbox.Sandbox) string { return sb.ID() }, page)

	data := make([]apitypes.Sandbox, 0, len(items))
	for _, sb := range items {
		data = append(data, toAPISandbox(sb))
	}

	writeJSON(w, http.StatusOK, apitypes.SandboxList{
		Object:  apitypes.SandboxListObjectList,
		Url:     "/" + APIVersion + "/sandboxes",
		HasMore: hasMore,
		Data:    data,
	})
}

func (s *Server) handleRetrieveSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPISandbox(sb))
}

// handleDeleteSandbox kills the VM and reports what it cost.
//
// It returns the sandbox rather than a bare 204, and that is the whole point:
// the final metering is sampled just before the kill, and once the VM is gone
// its cgroup goes with it. This reply is the only chance anyone has to learn
// what the sandbox consumed.
func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	// Its own deadline: a caller who hangs up mid-delete must not leave a VM
	// half-torn-down, holding a TAP device and a network slot forever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()

	if err := sb.Stop(ctx, sandbox.ReasonStopped); err != nil {
		s.log.Error("stop sandbox failed", "sandbox", sb.ID(), "err", err)
		s.writeAPIError(w, r, internalError(err))
		return
	}
	writeJSON(w, http.StatusOK, toAPISandbox(sb))
}

// sandbox resolves the {sandbox} path value.
func (s *Server) sandbox(r *http.Request) (*sandbox.Sandbox, error) {
	raw := r.PathValue("sandbox")

	// Check the shape before the lookup, so an execution ID pasted into a
	// sandbox route is reported as exactly that rather than as a missing object
	// the caller then hunts for.
	if err := id.Parse(raw, id.SandboxPrefix); err != nil {
		return nil, invalidParamError("sandbox", err.Error())
	}

	sb, ok := s.mgr.Get(raw)
	if !ok {
		return nil, notFoundError(CodeSandboxNotFound, "sandbox", raw)
	}
	return sb, nil
}

func toAPISandbox(sb *sandbox.Sandbox) apitypes.Sandbox {
	info := sb.Info()

	out := apitypes.Sandbox{
		Id:      info.ID,
		Object:  apitypes.SandboxObjectSandbox,
		Image:   info.Image,
		State:   apitypes.SandboxState(info.State),
		Created: info.CreatedAt,
		Expires: info.ExpiresAt,
		Vcpus:   ptr(info.VCPUs),
		MemMib:  ptr(info.MemMiB),
		Network: ptr(info.Network),
		Stats: apitypes.Stats{
			ActiveCpuMs:        int(info.Stats.ActiveCPU.Milliseconds()),
			WallMs:             int(info.Stats.Wall.Milliseconds()),
			IdleMs:             int(info.Stats.Idle.Milliseconds()),
			MemoryCurrentBytes: int(info.Stats.MemoryCurrent),
			MemoryPeakBytes:    int(info.Stats.MemoryPeak),
		},
	}
	if !info.StoppedAt.IsZero() {
		out.Stopped = &info.StoppedAt
	}
	if info.Reason != "" {
		reason := apitypes.SandboxStopReason(info.Reason)
		out.StopReason = &reason
	}
	if info.Storage != nil {
		st := &apitypes.SandboxStorage{
			Namespace: info.Storage.Namespace,
			ReadOnly:  info.Storage.ReadOnly,
		}
		if info.Storage.MountPath != "" {
			st.MountPath = ptr(info.Storage.MountPath)
		}
		if info.Storage.MaxBytes > 0 {
			st.MaxBytes = ptr(info.Storage.MaxBytes)
			policy := apitypes.TenantFullPolicy(info.Storage.Policy)
			st.Policy = &policy
		}
		out.Storage = st
	}
	return out
}

// ptr and deref bridge the generated types' optional fields.
//
// The generator makes every non-required field a pointer, which is the only way
// to tell "absent" from "zero" -- and that distinction is real here: an absent
// idle_seconds means the default, whereas an explicit 0 does not.
func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
