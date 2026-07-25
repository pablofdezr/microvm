package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/id"
	"github.com/pablofdezr/microvm/internal/runtime"
	"github.com/pablofdezr/microvm/internal/sandbox"
	"github.com/pablofdezr/microvm/internal/storage"
)

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	// The tag count, before the body becomes a map. ValidateTags below is still the
	// rule -- it owns the byte limits and the colon -- but it can only read a map
	// that already exists, and building it is where the cost is: 32 MiB of
	// `"0":"",` is two and a half million pairs and 140 MiB of live heap, allocated
	// only to be refused. See overMembers.
	if err := refuseTagFlood(w, r); err != nil {
		s.writeAPIError(w, r, err)
		return
	}

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
	// The name is a handle a caller picks, so it is validated here like a tag:
	// bounded charset and length, refused before anything is spent on the create.
	if params.Name != nil && *params.Name != "" {
		if err := sandbox.ValidateName(*params.Name); err != nil {
			s.writeAPIError(w, r, invalidParamError("name", err.Error()))
			return
		}
		spec.Name = *params.Name
	}
	getOrCreate := deref(params.GetOrCreate)
	if getOrCreate && spec.Name == "" {
		s.writeAPIError(w, r, invalidParamError("get_or_create",
			"needs a name to resolve; set name to the handle you want to reuse."))
		return
	}
	if params.Env != nil {
		spec.Env = *params.Env
	}
	if params.Tags != nil {
		// Bounded here, before anything holds them: tags live as long as the
		// sandbox does, so this is the one moment a caller can be told no.
		if err := sandbox.ValidateTags(*params.Tags); err != nil {
			s.writeAPIError(w, r, invalidParamError("tags", err.Error()))
			return
		}
		spec.Tags = *params.Tags
	}
	// The shape of the source, not its policy: whether this host may be fetched
	// from is the operator's answer and the core's to give. See source.go.
	if params.Source != nil {
		req, apiErr := validateSource(params.Source)
		if apiErr != nil {
			s.writeAPIError(w, r, apiErr)
			return
		}
		spec.Source = req
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
		// Who to attribute the sandbox to, and how many of them they may have.
		// From the token, like the namespace below and for the same reason: a
		// tenant a body could name is a limit a body could spend elsewhere.
		spec.Tenant = p.Tenant
		spec.MaxConcurrent = p.MaxConcurrent

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

	// GetOrCreate resolves an existing sandbox to a 200; a plain create always
	// boots and answers 201. Both share every failure path below.
	sb, created, err := s.createSandbox(r, spec, getOrCreate)
	if err != nil {
		// A name collision is the caller's to fix -- pick another name, or ask for
		// get_or_create -- so it is answered before the capacity paths that a
		// generic create failure falls through to.
		var nameConflict *sandbox.NameConflictError
		if errors.As(err, &nameConflict) {
			s.writeAPIError(w, r, conflictError(CodeAlreadyExists, fmt.Sprintf(
				"You already have a running sandbox named %q. Pass get_or_create to reuse it, "+
					"or choose another name.", nameConflict.Name)))
			return
		}
		// Seeding next, because a create that got as far as fetching failed for a
		// reason of its own: a refused URL is the caller's, a bad origin is somebody
		// else's server, and neither is this node being full. Reported as capacity
		// they would send a caller into a retry loop against a URL that will never
		// work.
		if seedErr := sourceCreateError(err); seedErr != nil {
			s.writeAPIError(w, r, seedErr)
			return
		}
		// The caller's own cap, not the node's. Same 429 so their retry logic is
		// unchanged, different message so they are not left waiting for room that
		// is already there.
		var limit *sandbox.ConcurrencyLimitError
		if errors.As(err, &limit) {
			s.writeAPIError(w, r, tenantConcurrencyError(err, limit.Live, limit.Max))
			return
		}
		// A failed create is nearly always the node being full. It is reported
		// as capacity rather than an internal error because the two call for
		// different reactions: back off and retry, or stop and read the logs.
		s.log.Warn("create sandbox failed", "image", params.Image, "err", err)
		s.writeAPIError(w, r, capacityError(err))
		return
	}

	// 201 for a sandbox that was booted, 200 for one get_or_create handed back
	// unchanged: a caller can tell "I made this" from "this already existed"
	// without diffing the body against what they sent.
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, toAPISandbox(sb))
}

// createSandbox routes a create to the plain or the get-or-create path and
// reports whether a VM was booted. The plain path always did, so its bool is
// true; get-or-create returns false when it resolved an existing sandbox.
func (s *Server) createSandbox(r *http.Request, spec sandbox.Spec, getOrCreate bool) (*sandbox.Sandbox, bool, error) {
	if getOrCreate {
		return s.mgr.GetOrCreate(r.Context(), spec)
	}
	sb, err := s.mgr.Create(r.Context(), spec)
	return sb, true, err
}

// refuseTagFlood bounds the tag count before the map that would hold it exists.
//
// It reads only `tags` and unknown fields are ignored, unlike the decode that
// follows: this pass is not the one that judges the body, and duplicating that
// judgement here would give the same mistake two different error messages.
func refuseTagFlood(w http.ResponseWriter, r *http.Request) error {
	body, err := readBody(w, r)
	if err != nil {
		return err
	}

	var envelope struct {
		Tags json.RawMessage `json:"tags"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil // malformed; the real decode reports it
	}
	if overMembers(envelope.Tags, sandbox.MaxTags) {
		return invalidParamError("tags", fmt.Sprintf("at most %d tags, and this is more.", sandbox.MaxTags))
	}
	return nil
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	page, err := parsePageParams(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	// Scoped before any filter, because a list is how an ID is found in the first
	// place: one that reports every tenant's sandboxes reports their tags and their
	// storage namespace too, and hands out the IDs every other route is reached by.
	all := s.mgr.List()
	mine := all[:0:0]
	for _, sb := range all {
		if s.mayReach(r.Context(), sb) {
			mine = append(mine, sb)
		}
	}
	all = mine

	if want := r.URL.Query().Get("state"); want != "" {
		filtered := all[:0:0]
		for _, sb := range all {
			if string(sb.Info().State) == want {
				filtered = append(filtered, sb)
			}
		}
		all = filtered
	}

	// Repeats AND, so a second tag narrows the page rather than widening it.
	// Unlike state, a value this cannot parse is refused: an unrecognised state
	// is a real question with an empty answer, whereas a tag with no colon names
	// no key to answer about.
	if raw := r.URL.Query()["tag"]; len(raw) > 0 {
		want, err := parseTagFilters(raw)
		if err != nil {
			s.writeAPIError(w, r, err)
			return
		}
		filtered := all[:0:0]
		for _, sb := range all {
			if hasTags(sb.Tags(), want) {
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

// tagFilter is one `key:value` a listed sandbox has to carry.
//
// A pair rather than a map, because two values for one key must both match and
// therefore match nothing -- an empty page, which is the honest answer. A map
// would drop one of them and quietly return the sandboxes carrying the other.
type tagFilter struct{ key, value string }

// parseTagFilters reads the repeated ?tag=key:value.
//
// The split is at the first colon, so a value may contain colons and a key may
// not -- which is also why a key containing one is refused at create time.
func parseTagFilters(raw []string) ([]tagFilter, *apiError) {
	out := make([]tagFilter, 0, len(raw))
	for _, t := range raw {
		key, value, found := strings.Cut(t, ":")
		if !found || key == "" {
			return nil, invalidParamError("tag", fmt.Sprintf(
				"%q is not a tag filter. Give key:value, e.g. \"env:ci\".", t))
		}
		out = append(out, tagFilter{key: key, value: value})
	}
	return out, nil
}

// hasTags reports whether tags carries every pair.
//
// A key nobody set is simply not a match. There is no registry of known keys, so
// there is nothing to be wrong about: an unused key is an empty page rather than
// an error, the same way an unused state is.
func hasTags(tags map[string]string, want []tagFilter) bool {
	for _, f := range want {
		if v, ok := tags[f.key]; !ok || v != f.value {
			return false
		}
	}
	return true
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

// maxExtendSeconds is the largest ttl_seconds worth reading at all.
//
// It bounds the number before it becomes a Duration, because a caller-chosen
// seconds count large enough to overflow one comes out negative -- and a
// negative extension reads downstream as "no change" rather than as the refusal
// it is. The real rule is the core's, measured from creation; this only refuses
// input that could not be honoured by any sandbox.
const maxExtendSeconds = int(sandbox.MaxTTL / time.Second)

// handleExtendSandbox pushes a sandbox's TTL deadline out.
//
// It returns the sandbox because `expires` on the reply is the only statement of
// the real deadline: the request says how long a caller wants, and this says
// what they have. A deadline is never brought forward, so a retry is harmless
// and the route takes no Idempotency-Key.
func (s *Server) handleExtendSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	var params apitypes.SandboxExtendParams
	if err := decodeBody(w, r, &params); err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	switch {
	case params.TtlSeconds == 0:
		s.writeAPIError(w, r, missingParamError("ttl_seconds"))
		return
	case params.TtlSeconds < 0:
		s.writeAPIError(w, r, invalidParamError("ttl_seconds",
			"an extension must be a positive number of seconds."))
		return
	case params.TtlSeconds > maxExtendSeconds:
		s.writeAPIError(w, r, invalidParamError("ttl_seconds", fmt.Sprintf(
			"a sandbox lives at most %d seconds from creation, so %d is more than any extension can buy.",
			maxExtendSeconds, params.TtlSeconds)))
		return
	}

	if _, err := sb.Extend(time.Duration(params.TtlSeconds) * time.Second); err != nil {
		// Past the lifetime bound is the caller's arithmetic, not our failure, and
		// it is refused rather than clamped: a caller who believes they have an
		// hour and has ten minutes is worse off than one who was told no.
		var limit *sandbox.TTLLimitError
		if errors.As(err, &limit) {
			s.writeAPIError(w, r, invalidParamError("ttl_seconds", fmt.Sprintf(
				"this sandbox lives at most %d seconds from creation, and %d of those are left. Asked for %d.",
				maxExtendSeconds, int(limit.Remaining.Seconds()), params.TtlSeconds)))
			return
		}
		// The deadline passed while this call was in flight and the supervisor has
		// already claimed the kill. Reported as the 409 a stopped sandbox gets,
		// because that is what the caller would have been told a millisecond later --
		// and the state cannot be read to work it out, since it still says running
		// for as long as the kill takes.
		var expired *sandbox.ExpiredError
		if errors.As(err, &expired) {
			s.writeAPIError(w, r, sandboxNotRunningError(
				sb.ID(), string(sandbox.StateStopped), string(sandbox.ReasonExpired)))
			return
		}
		s.writeAPIError(w, r, s.sandboxStateError(sb, err))
		return
	}

	writeJSON(w, http.StatusOK, toAPISandbox(sb))
}

// handleSuspendSandbox snapshots the sandbox's VM and tears it down, leaving it
// resumable under the same id.
func (s *Server) handleSuspendSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	// Its own deadline, generous: the snapshot write is the guest's whole memory
	// to disk, which is minutes on a large guest or a slow disk (see fcapi). A
	// client hanging up mid-capture must not abort it and leave the VM in limbo.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 6*time.Minute)
	defer cancel()

	if err := sb.Suspend(ctx); err != nil {
		switch {
		case errors.Is(err, sandbox.ErrSnapshotUnsupported):
			s.writeAPIError(w, r, conflictError(CodeSnapshotUnsupported,
				"This node is not configured for suspend/resume."))
		case errors.Is(err, sandbox.ErrSandboxBusy):
			s.writeAPIError(w, r, conflictError(CodeSandboxBusy,
				"This sandbox has an execution in flight. Finish or cancel it, then suspend."))
		case sb.State() != sandbox.StateRunning:
			// Not running to begin with, or the capture failed and left it stopped.
			s.writeAPIError(w, r, sandboxNotRunningError(sb.ID(), string(sb.State()), string(sb.Reason())))
		default:
			s.log.Error("suspend sandbox failed", "sandbox", sb.ID(), "err", err)
			s.writeAPIError(w, r, internalError(err))
		}
		return
	}
	writeJSON(w, http.StatusOK, toAPISandbox(sb))
}

// handleResumeSandbox boots a fresh VM from a suspended sandbox's snapshot.
func (s *Server) handleResumeSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	if sb.State() != sandbox.StateSuspended {
		s.writeAPIError(w, r, conflictError(CodeNotSuspended, fmt.Sprintf(
			"Sandbox %s is %s, not suspended, so there is nothing to resume.", sb.ID(), sb.State())))
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()

	resumed, err := s.mgr.Resume(ctx, sb)
	if err != nil {
		if errors.Is(err, sandbox.ErrSnapshotUnsupported) {
			s.writeAPIError(w, r, conflictError(CodeSnapshotUnsupported,
				"This node is not configured for suspend/resume."))
			return
		}
		// Booting the VM back from the snapshot failed. Reported as capacity, like a
		// create that could not boot: the caller's move is to back off and retry, and
		// the snapshot is still there to retry against.
		s.log.Warn("resume sandbox failed", "sandbox", sb.ID(), "err", err)
		s.writeAPIError(w, r, capacityError(err))
		return
	}
	writeJSON(w, http.StatusOK, toAPISandbox(resumed))
}

// sandbox resolves the {sandbox} path value, scoped to whoever is asking.
func (s *Server) sandbox(r *http.Request) (*sandbox.Sandbox, error) {
	raw := r.PathValue("sandbox")

	// Check the shape before the lookup, so an execution ID pasted into a
	// sandbox route is reported as exactly that rather than as a missing object
	// the caller then hunts for.
	if err := id.Parse(raw, id.SandboxPrefix); err != nil {
		return nil, invalidParamError("sandbox", err.Error())
	}

	sb, ok := s.mgr.Get(raw)
	if !ok || !s.mayReach(r.Context(), sb) {
		return nil, notFoundError(CodeSandboxNotFound, "sandbox", raw)
	}
	return sb, nil
}

// mayReach reports whether the caller owns this sandbox.
//
// Every route that resolves a sandbox goes through here, because an ID is not a
// capability: without it any valid token could exec in, extend, delete, or read
// files out of another tenant's sandbox -- and the files include that tenant's
// whole object-store namespace, which is mounted inside it. Extending someone
// else's sandbox is worse than reading it, since the slot it holds is charged to
// their concurrency cap and not to the caller's.
//
// The refusal is a 404 and not a 403, so IDs stay unenumerable: a 403 would
// confirm which of a guessed range exist.
//
// Two identities are deliberately unscoped. With auth disabled there is no
// identity to scope to, which is the same case where storage falls back to a
// per-sandbox namespace. An admin key keeps the node-wide view, because it is the
// operator's own key -- it already decides what every tenant may store -- and
// taking the view away would leave nothing able to answer "what is on this box".
func (s *Server) mayReach(ctx context.Context, sb *sandbox.Sandbox) bool {
	p := principalFrom(ctx)
	if p == nil || p.Admin {
		return true
	}
	return sb.Tenant() == p.Tenant
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
	// Absent rather than zero when the sandbox has no TAP to read: a sandbox
	// created with network: false transferred nothing measurable, which is a
	// different statement from having measured zero bytes. The runtime hands these
	// over already turned round to the guest's point of view, so tx is egress.
	if v := info.Stats.NetworkRxBytes; v != nil {
		out.Stats.NetworkRxBytes = ptr(int64(*v))
	}
	if v := info.Stats.NetworkTxBytes; v != nil {
		out.Stats.NetworkTxBytes = ptr(int64(*v))
	}

	// Absent rather than empty when the sandbox has no name: a caller reading this
	// back sees the same shape they sent, and an anonymous sandbox has no name
	// field rather than an empty one.
	if info.Name != "" {
		out.Name = ptr(info.Name)
	}
	if !info.StoppedAt.IsZero() {
		out.Stopped = &info.StoppedAt
	}
	if info.Reason != "" {
		reason := apitypes.SandboxStopReason(info.Reason)
		out.StopReason = &reason
	}
	// Absent rather than an empty object when there are none: a caller reading
	// this back should see the same shape they sent.
	if len(info.Tags) > 0 {
		out.Tags = &info.Tags
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
	out.Source = toAPISource(info.Source)
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
