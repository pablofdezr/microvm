package source

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestSeeder points a Seeder at an httptest server, exactly as
// newTestFetcher does and for the same reasons: the server listens on loopback
// with a self-signed certificate, and the daemon refuses both.
func newTestSeeder(t *testing.T, srv *httptest.Server, cfg Config) *Seeder {
	t.Helper()

	s, err := NewSeeder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if srv != nil {
		s.fetcher = newTestFetcher(t, srv, cfg)
	}
	s.insecure = true
	s.fetcher.permitLoopback = true
	return s
}

func TestPrepareATarball(t *testing.T) {
	srv := serveArchive(t, sampleTarGz(t))
	s := newTestSeeder(t, srv, Config{})

	prepared, err := s.Prepare(context.Background(), Request{
		Type:            TypeTarball,
		URL:             srv.URL + "/app.tar.gz?X-Amz-Signature=topsecret",
		StripComponents: 1,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer prepared.Close()

	// The manifest is readable before anything is written, which is what lets the
	// layer above refuse a tree too big for the sandbox before booting one.
	if man := prepared.Manifest(); man.Files != 2 {
		t.Errorf("Files = %d, want 2", man.Files)
	}

	res := prepared.Result()
	if res.Type != TypeTarball {
		t.Errorf("Type = %q, want tarball", res.Type)
	}
	if strings.Contains(res.URLRedacted, "topsecret") {
		t.Errorf("the reported URL carries the query string: %q", res.URLRedacted)
	}
	if res.Files != 2 || res.Bytes == 0 {
		t.Errorf("Result = %+v, want the manifest's counts", res)
	}

	var w recordingWriter
	if err := prepared.Write(context.Background(), &w); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := w.files["main.go"]; got.content != "package main\n" {
		t.Errorf("main.go = %q", got.content)
	}
	if got := w.files["run.sh"]; got.mode != "0755" {
		t.Errorf("run.sh mode = %q, want 0755", got.mode)
	}
}

// Nothing is prepared until an operator names a host, whatever the type. This is
// the off state, and it is the state an existing deployment stays in.
func TestPrepareWithNoAllowlistRefusesEverything(t *testing.T) {
	s, err := NewSeeder(Config{})
	if err != nil {
		t.Fatal(err)
	}

	for _, req := range []Request{
		{Type: TypeTarball, URL: "https://example.com/app.tar.gz"},
		{Type: TypeGit, URL: "https://example.com/repo.git"},
	} {
		if _, err := s.Prepare(context.Background(), req); !errors.Is(err, ErrNotPermitted) {
			t.Errorf("Prepare(%s) = %v, want ErrNotPermitted", req.Type, err)
		}
	}
}

func TestPrepareRefusesAnUnknownType(t *testing.T) {
	s := newTestSeeder(t, nil, Config{AllowHosts: []string{"example.com"}})

	_, err := s.Prepare(context.Background(), Request{Type: "svn", URL: "https://example.com/x"})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("Prepare with an unknown type = %v, want ErrNotPermitted", err)
	}
}

// A tarball that cannot be expanded is refused by Prepare, before the caller has
// booted anything -- and the buffer goes with it rather than being held until
// somebody remembers to Close a Prepared they never got.
func TestPrepareValidatesBeforeReturning(t *testing.T) {
	srv := serveArchive(t, []byte("this is not a tar at all, not even close"))
	s := newTestSeeder(t, srv, Config{})

	_, err := s.Prepare(context.Background(), Request{Type: TypeTarball, URL: srv.URL + "/x.tar"})
	if !errors.Is(err, ErrInvalidArchive) {
		t.Fatalf("Prepare of a non-archive = %v, want ErrInvalidArchive", err)
	}
}
