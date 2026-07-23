package api

import (
	"bytes"
	"io"
	"net/http"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
)

func (s *Server) handleCreateFile(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	var params apitypes.FileCreateParams
	if err := decodeBody(w, r, &params); err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	if params.Path == "" {
		s.writeAPIError(w, r, missingParamError("path"))
		return
	}

	if err := sb.WriteFile(r.Context(), params.Path, bytes.NewReader(params.Content), ""); err != nil {
		s.writeAPIError(w, r, s.sandboxStateError(sb, err))
		return
	}

	writeJSON(w, http.StatusCreated, apitypes.File{
		Object:    apitypes.FileObjectFile,
		Path:      params.Path,
		SizeBytes: len(params.Content),
	})
}

// handleRetrieveFile streams a file out of the sandbox.
//
// The body is the file's bytes rather than a JSON object wrapping them. A
// resource whose entire content is one blob should be served as that blob:
// base64 in JSON would inflate it by a third and force a caller to buffer the
// whole thing before seeing the first byte.
func (s *Server) handleRetrieveFile(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		s.writeAPIError(w, r, missingParamError("path"))
		return
	}

	rc, err := sb.ReadFile(r.Context(), path)
	if err != nil {
		if sb.State() != "running" {
			s.writeAPIError(w, r, s.sandboxStateError(sb, err))
			return
		}
		s.writeAPIError(w, r, notFoundError(CodeFileNotFound, "file", path))
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	// The status is committed the moment the first byte goes out, so a copy that
	// fails halfway can only be logged -- there is no way left to tell the
	// caller, and pretending otherwise would corrupt the body.
	if _, err := io.Copy(w, rc); err != nil {
		s.log.Warn("file download interrupted",
			"request_id", requestIDFrom(r.Context()),
			"sandbox", sb.ID(), "path", path, "err", err)
	}
}
