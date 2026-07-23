package api

import (
	"net/http"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/storage"
)

// handleUpdateTenant sets a tenant's storage policy. Admin only, gated upstream.
func (s *Server) handleUpdateTenant(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Tenants == nil {
		s.writeAPIError(w, r, notFoundError(CodeRouteNotFound, "tenant policy store", "this node stores no tenant policies"))
		return
	}

	tenant := r.PathValue("tenant")
	var params apitypes.TenantUpdateParams
	if err := decodeBody(w, r, &params); err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	policy, err := toPolicy(params.MaxBytes, params.Policy)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	if err := s.cfg.Tenants.Set(r.Context(), tenant, policy); err != nil {
		s.writeAPIError(w, r, internalError(err))
		return
	}

	// No usage on the update reply: setting a policy should not pay for a bucket
	// scan the caller did not ask for. Retrieve returns usage.
	writeJSON(w, http.StatusOK, toAPITenant(tenant, policy, nil))
}

// handleRetrieveTenant returns a tenant's policy and current usage.
func (s *Server) handleRetrieveTenant(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Tenants == nil {
		s.writeAPIError(w, r, notFoundError(CodeRouteNotFound, "tenant policy store", "this node stores no tenant policies"))
		return
	}

	tenant := r.PathValue("tenant")
	policy, ok, err := s.cfg.Tenants.Get(r.Context(), tenant)
	if err != nil {
		s.writeAPIError(w, r, internalError(err))
		return
	}
	if !ok {
		s.writeAPIError(w, r, notFoundError(CodeTenantNotFound, "tenant policy", tenant))
		return
	}

	// Usage is read from the store: the real total, worth the scan on a call an
	// admin makes deliberately.
	used, err := s.mgr.TenantUsage(r.Context(), tenant)
	if err != nil {
		s.log.Warn("read tenant usage failed", "tenant", tenant, "err", err)
		// The policy is still worth returning; usage is best-effort.
		writeJSON(w, http.StatusOK, toAPITenant(tenant, policy, nil))
		return
	}
	writeJSON(w, http.StatusOK, toAPITenant(tenant, policy, &used))
}

// handleListTenants returns every configured tenant.
func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Tenants == nil {
		writeJSON(w, http.StatusOK, apitypes.TenantList{
			Object:  apitypes.List,
			Url:     "/" + APIVersion + "/tenants",
			HasMore: false,
			Data:    []apitypes.Tenant{},
		})
		return
	}

	records, err := s.cfg.Tenants.List(r.Context())
	if err != nil {
		s.writeAPIError(w, r, internalError(err))
		return
	}

	data := make([]apitypes.Tenant, 0, len(records))
	for _, rec := range records {
		data = append(data, toAPITenant(rec.Tenant, rec.Policy, nil))
	}
	writeJSON(w, http.StatusOK, apitypes.TenantList{
		Object:  apitypes.List,
		Url:     "/" + APIVersion + "/tenants",
		HasMore: false,
		Data:    data,
	})
}

// toPolicy converts wire params into a domain policy, validating the enum.
func toPolicy(maxBytes int64, policy apitypes.TenantFullPolicy) (storage.TenantPolicy, error) {
	if maxBytes < 0 {
		return storage.TenantPolicy{}, invalidParamError("max_bytes", "must not be negative")
	}
	switch policy {
	case apitypes.Preserve:
		return storage.TenantPolicy{MaxBytes: maxBytes, OnFull: storage.Preserve}, nil
	case apitypes.Evict:
		return storage.TenantPolicy{MaxBytes: maxBytes, OnFull: storage.Evict}, nil
	default:
		return storage.TenantPolicy{}, invalidParamError("policy", "must be 'preserve' or 'evict'")
	}
}

func toAPITenant(id string, policy storage.TenantPolicy, usage *int64) apitypes.Tenant {
	out := apitypes.Tenant{
		Object:   apitypes.TenantObjectTenant,
		Id:       id,
		MaxBytes: policy.MaxBytes,
		Policy:   apitypes.TenantFullPolicy(policy.OnFull),
	}
	if usage != nil {
		out.UsageBytes = usage
	}
	return out
}
