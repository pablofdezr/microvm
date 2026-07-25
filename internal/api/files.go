package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/guestclient"
	"github.com/pablofdezr/microvm/internal/sandbox"
)

// defaultFileMode is what an upload gets when the caller does not choose.
//
// It is decided here rather than left to the guest because the reply states the
// mode the file ended up with: a default that lives on the far side of the vsock
// is a default this layer would have to guess at to report it.
const defaultFileMode = "0644"

// maxBatchFiles caps one batch write. A hundred files is a project; a thousand
// is a caller using the API as a filesystem, and every entry is a vsock round
// trip the request holds a slot open for.
const maxBatchFiles = 100

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

	file, apiErr := validateFile(params, "")
	if apiErr != nil {
		s.writeAPIError(w, r, apiErr)
		return
	}

	if err := sb.WriteFile(r.Context(), file.Path, bytes.NewReader(params.Content), *file.Mode); err != nil {
		s.writeAPIError(w, r, s.sandboxStateError(sb, err))
		return
	}

	writeJSON(w, http.StatusCreated, file)
}

// handleCreateFiles writes a set of files in one request.
//
// Validation is all-or-nothing and writing is not, because writing cannot be:
// there is no unwriting the third file. So every entry is checked before the
// first byte reaches the guest -- a batch with one bad mode writes nothing -- the
// files go in the order given, and a failure names the entry it stopped at. Those
// two together are what let a caller whose write failed halfway say which ones
// landed.
//
// A path may be claimed once. Two entries for one path have no order that makes
// the reply true: it would report two sizes for one file, and only the later of
// them is on disk.
func (s *Server) handleCreateFiles(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	files, err := decodeFileBatch(w, r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	if len(files) == 0 {
		s.writeAPIError(w, r, missingParamError("files"))
		return
	}

	data := make([]apitypes.File, 0, len(files))
	// Written paths, so far, and which entry claimed each. Two entries for one path
	// would report two sizes for one file and leave only the later on disk, which
	// makes the reply a statement about nothing.
	claimed := make(map[string]int, len(files))
	for i, entry := range files {
		file, apiErr := validateFile(entry, fmt.Sprintf("files[%d].", i))
		if apiErr != nil {
			s.writeAPIError(w, r, apiErr)
			return
		}
		if first, dup := claimed[file.Path]; dup {
			s.writeAPIError(w, r, invalidParamError(fmt.Sprintf("files[%d].path", i), fmt.Sprintf(
				"%s is already written by files[%d]. A batch names each path once.", file.Path, first)))
			return
		}
		claimed[file.Path] = i
		data = append(data, file)
	}

	for i, file := range data {
		if err := sb.WriteFile(r.Context(), file.Path, bytes.NewReader(files[i].Content), *file.Mode); err != nil {
			s.writeAPIError(w, r, s.batchWriteError(sb, i, file.Path, err))
			return
		}
	}

	writeJSON(w, http.StatusCreated, apitypes.FileList{
		Object:  apitypes.FileListObjectList,
		Url:     fmt.Sprintf("/%s/sandboxes/%s/files/batch", APIVersion, sb.ID()),
		HasMore: false,
		Data:    data,
	})
}

// decodeFileBatch reads a batch body, refusing an over-long array before its
// elements exist.
//
// decodeBody applies the daemon's body cap, which is the whole batch's size limit:
// the files arrive as one request, base64 and all. What it cannot do is bound the
// count, and the count is where the amplification lives -- see overMembers. So the
// array arrives as raw bytes, is counted, and only then becomes elements.
func decodeFileBatch(w http.ResponseWriter, r *http.Request) ([]apitypes.FileCreateParams, error) {
	// The envelope carries exactly the fields FileBatchCreateParams does, so an
	// unknown one is still refused here; only `files` is held back.
	var envelope struct {
		Files json.RawMessage `json:"files"`
	}
	if err := decodeBody(w, r, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Files) == 0 {
		return nil, missingParamError("files")
	}
	if overMembers(envelope.Files, maxBatchFiles) {
		return nil, invalidParamError("files", fmt.Sprintf(
			"a batch holds at most %d files, and this one holds more.", maxBatchFiles))
	}

	var files []apitypes.FileCreateParams
	dec := json.NewDecoder(bytes.NewReader(envelope.Files))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&files); err != nil {
		return nil, invalidBodyError(err)
	}
	return files, nil
}

// batchWriteError says where a partly-written batch stopped.
//
// The route promises the files go in the order given, which is what lets a caller
// whose write failed halfway work out which ones landed -- and that promise is only
// usable if the failure names the entry it stopped at. The code and type are
// whatever a single upload would have produced, since the reason is the same one.
func (s *Server) batchWriteError(sb *sandbox.Sandbox, i int, path string, cause error) error {
	err := s.sandboxStateError(sb, cause)

	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return err
	}
	// A copy: the constructors hand back a fresh value, and appending to a shared
	// one would rewrite the message every later caller reads.
	stopped := *apiErr
	stopped.message = fmt.Sprintf("%s The batch stopped at files[%d] (%s); the entries before it were written.",
		apiErr.message, i, path)
	return &stopped
}

// handleCreateDir creates a directory and its parents.
//
// Uploading a file already creates its parents, so this route is for the
// directory that stays empty: somewhere a command writes its output, or a layout
// a build tool expects before it starts.
func (s *Server) handleCreateDir(w http.ResponseWriter, r *http.Request) {
	sb, err := s.sandbox(r)
	if err != nil {
		s.writeAPIError(w, r, err)
		return
	}

	var params apitypes.DirectoryCreateParams
	if err := decodeBody(w, r, &params); err != nil {
		s.writeAPIError(w, r, err)
		return
	}
	if apiErr := validateDirPath(params.Path); apiErr != nil {
		s.writeAPIError(w, r, apiErr)
		return
	}

	if err := sb.Mkdir(r.Context(), params.Path); err != nil {
		// A directory that already exists is success -- mkdir -p, so a retry is
		// safe. A *file* in the way is not: that is a caller confusing two
		// different things, and telling them so beats a 500 that blames us.
		if errors.Is(err, guestclient.ErrNotDirectory) {
			s.writeAPIError(w, r, conflictError(CodeAlreadyExists,
				fmt.Sprintf("%s already exists and is not a directory.", params.Path)))
			return
		}
		s.writeAPIError(w, r, s.sandboxStateError(sb, err))
		return
	}

	writeJSON(w, http.StatusCreated, apitypes.Directory{
		Object: apitypes.DirectoryObjectDirectory,
		Path:   params.Path,
	})
}

// validateFile checks one upload and returns the resource it will produce.
//
// The batch route runs this over every entry before it writes any of them, which
// is the reason it exists as a function: one file and a hundred must be refused
// for the same reasons, and two copies of these rules would eventually disagree.
// prefix names the entry a batch is complaining about ("files[3]."), and is
// empty for a single upload.
func validateFile(params apitypes.FileCreateParams, prefix string) (apitypes.File, *apiError) {
	if params.Path == "" {
		return apitypes.File{}, missingParamError(prefix + "path")
	}

	mode := defaultFileMode
	if params.Mode != nil {
		normalised, apiErr := parseFileMode(prefix+"mode", *params.Mode)
		if apiErr != nil {
			return apitypes.File{}, apiErr
		}
		mode = normalised
	}

	return apitypes.File{
		Object:    apitypes.FileObjectFile,
		Path:      params.Path,
		SizeBytes: len(params.Content),
		Mode:      &mode,
	}, nil
}

// parseFileMode validates an octal mode and returns it normalised to four
// digits, which is both what the guest is told and what the reply reports.
//
// Parsed by hand rather than with strconv, because the shape is part of the
// contract: ParseUint would take "7" and "07777" just as happily.
func parseFileMode(param, mode string) (string, *apiError) {
	if n := len(mode); n < 3 || n > 4 {
		return "", invalidParamError(param, notAFileMode(mode))
	}

	var bits uint32
	for i := 0; i < len(mode); i++ {
		if mode[i] < '0' || mode[i] > '7' {
			return "", invalidParamError(param, notAFileMode(mode))
		}
		bits = bits<<3 | uint32(mode[i]-'0')
	}

	// setuid, setgid and the sticky bit. The host writes this file as root, so
	// the one thing an upload must never be able to do is leave a root-owned
	// setuid binary behind for whatever runs in the guest next. Refused rather
	// than masked off: answering 201 to a request we did not honour is worse
	// than refusing it.
	if bits&^0o777 != 0 {
		return "", invalidParamError(param, fmt.Sprintf(
			"%q sets the setuid, setgid or sticky bit, which an upload may not do. "+
				"Code inside the sandbox can still chmod its own files.", mode))
	}

	return fmt.Sprintf("0%03o", bits), nil
}

func notAFileMode(mode string) string {
	return fmt.Sprintf("%q is not a file mode. Give three octal digits, e.g. \"0644\".", mode)
}

// validateDirPath refuses a path rather than resolving one nobody meant.
//
// Uploads have always taken a relative path and joined it to the working
// directory, so that route cannot start refusing them. This one is new and can
// hold the line the spec draws: absolute, and no ".." to resolve.
func validateDirPath(path string) *apiError {
	if path == "" {
		return missingParamError("path")
	}
	if !strings.HasPrefix(path, "/") {
		return invalidParamError("path", fmt.Sprintf(
			"%q is relative. Give an absolute path, e.g. \"/app/out\".", path))
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return invalidParamError("path", fmt.Sprintf("%q contains \"..\".", path))
		}
	}
	return nil
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
