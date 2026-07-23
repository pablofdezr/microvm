package microvm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	microvm "github.com/pablofdezr/microvm-sdk-go/microvm"
)

// tenantServer is a stand-in daemon that records the last request and answers
// the tenant routes. The SDK is a thin wrapper over generated types, so what is
// worth testing is exactly this seam: that each method hits the right verb and
// path and decodes the reply -- not the transport, which every method shares.
func tenantServer(t *testing.T) (*httptest.Server, *lastRequest) {
	t.Helper()
	last := &lastRequest{}
	mux := http.NewServeMux()

	mux.HandleFunc("PUT /v1/tenants/{id}", func(w http.ResponseWriter, r *http.Request) {
		last.record(r)
		if r.Header.Get("Authorization") != "Bearer admin" {
			forbidden(w)
			return
		}
		writeJSON(w, map[string]any{
			"id": r.PathValue("id"), "object": "tenant",
			"max_bytes": last.body["max_bytes"], "policy": last.body["policy"],
		})
	})
	mux.HandleFunc("GET /v1/tenants/{id}", func(w http.ResponseWriter, r *http.Request) {
		last.record(r)
		writeJSON(w, map[string]any{
			"id": r.PathValue("id"), "object": "tenant",
			"max_bytes": 1048576, "policy": "preserve", "usage_bytes": 4096,
		})
	})
	mux.HandleFunc("GET /v1/tenants", func(w http.ResponseWriter, r *http.Request) {
		last.record(r)
		writeJSON(w, map[string]any{
			"object": "list", "url": "/v1/tenants", "has_more": false,
			"data": []map[string]any{
				{"id": "t_a", "object": "tenant", "max_bytes": 100, "policy": "evict"},
				{"id": "t_b", "object": "tenant", "max_bytes": 0, "policy": "preserve"},
			},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, last
}

func TestTenantSetLimit(t *testing.T) {
	srv, last := tenantServer(t)
	client := microvm.New(srv.URL, microvm.WithToken("admin"))

	tn, err := client.Tenants.SetLimit(context.Background(), "t_a", 1<<20, microvm.Evict)
	if err != nil {
		t.Fatal(err)
	}
	if last.method != http.MethodPut || last.path != "/v1/tenants/t_a" {
		t.Errorf("hit %s %s, want PUT /v1/tenants/t_a", last.method, last.path)
	}
	// The body must carry both fields, or the server would apply a zero policy.
	if last.body["max_bytes"].(float64) != float64(1<<20) || last.body["policy"] != "evict" {
		t.Errorf("body = %+v, want max_bytes=1MiB policy=evict", last.body)
	}
	if tn.MaxBytes != 1<<20 || tn.Policy != microvm.Evict {
		t.Errorf("decoded %+v, want the values sent back", tn)
	}
}

func TestTenantRetrieveReportsUsage(t *testing.T) {
	srv, _ := tenantServer(t)
	client := microvm.New(srv.URL, microvm.WithToken("admin"))

	tn, err := client.Tenants.Retrieve(context.Background(), "t_x")
	if err != nil {
		t.Fatal(err)
	}
	if tn.UsageBytes == nil || *tn.UsageBytes != 4096 {
		t.Errorf("usage = %v, want 4096", tn.UsageBytes)
	}
}

func TestTenantList(t *testing.T) {
	srv, _ := tenantServer(t)
	client := microvm.New(srv.URL, microvm.WithToken("admin"))

	list, err := client.Tenants.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 2 {
		t.Fatalf("listed %d tenants, want 2", len(list.Data))
	}
}

// TestTenantUpdateForbiddenIsTyped confirms the 403 an ordinary key gets is
// recognisable, so a caller can tell "you may not" apart from "no such tenant".
func TestTenantUpdateForbiddenIsTyped(t *testing.T) {
	srv, _ := tenantServer(t)
	client := microvm.New(srv.URL, microvm.WithToken("not-admin"))

	_, err := client.Tenants.SetLimit(context.Background(), "t_a", 1<<20, microvm.Preserve)
	if err == nil {
		t.Fatal("a non-admin update should have failed")
	}
	if !microvm.IsForbidden(err) {
		t.Errorf("err = %v, want IsForbidden", err)
	}
}

// --- test helpers ---------------------------------------------------------

type lastRequest struct {
	method string
	path   string
	body   map[string]any
}

func (l *lastRequest) record(r *http.Request) {
	l.method = r.Method
	l.path = r.URL.Path
	l.body = map[string]any{}
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &l.body)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type": "authentication_error", "code": "forbidden",
			"message": "this key may not set tenant policies",
		},
	})
}
