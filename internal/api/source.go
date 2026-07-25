package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pablofdezr/microvm/internal/api/apitypes"
	"github.com/pablofdezr/microvm/internal/sandbox"
	"github.com/pablofdezr/microvm/internal/source"
)

// The `source` half of a create request: what this layer refuses, and how a
// refusal from the core reaches the caller.
//
// The shape is judged here and nothing else is. Whether a host may be fetched
// from, where its name resolves, and how big an archive may be are the operator's
// policy and live in internal/source -- repeated here they would apply to callers
// who came through this adapter and to nobody else.

// Field limits, from the spec. A generated type carries no maxLength, so these
// are the only thing enforcing it, and each one bounds something a request would
// otherwise be able to make arbitrarily long: the URL is stored on the sandbox and
// echoed back, the ref becomes an argument to git, the credential name becomes a
// map lookup.
const (
	maxSourceURLBytes           = 2048
	maxSourceRefBytes           = 255
	maxSourceCredentialRefBytes = 128
)

// validateSource turns the source half of a create body into a request the core
// can act on, or refuses it naming the field at fault.
//
// A per-type field sent with the other type is an error rather than a field
// quietly dropped. A caller who set `ref` on a tarball meant something by it, and
// seeding the default branch instead is the wrong project delivered silently --
// the worst failure this feature can have, because everything afterwards looks
// like it worked.
func validateSource(p *apitypes.SandboxSourceParams) (*source.Request, *apiError) {
	if p.Type == "" {
		return nil, missingParamError("source.type")
	}
	if !p.Type.Valid() {
		return nil, invalidParamError("source.type", fmt.Sprintf(
			"%q is not a source type. Give \"tarball\" or \"git\".", p.Type))
	}

	url := strings.TrimSpace(p.Url)
	switch {
	case url == "":
		return nil, missingParamError("source.url")
	case len(url) > maxSourceURLBytes:
		return nil, invalidParamError("source.url", fmt.Sprintf(
			"a URL may be at most %d bytes, and this is %d.", maxSourceURLBytes, len(url)))
	}

	req := &source.Request{Type: source.Type(p.Type), URL: url}

	switch req.Type {
	case source.TypeTarball:
		if p.Ref != nil {
			return nil, wrongSourceTypeError("source.ref", "git", "tarball")
		}
		if p.Depth != nil {
			return nil, wrongSourceTypeError("source.depth", "git", "tarball")
		}
		if p.CredentialRef != nil {
			return nil, wrongSourceTypeError("source.credential_ref", "git", "tarball")
		}
		if p.StripComponents != nil {
			if *p.StripComponents < 0 || *p.StripComponents > source.MaxStripComponents {
				return nil, invalidParamError("source.strip_components", fmt.Sprintf(
					"must be between 0 and %d. Past that it is not a checkout layout, it is a typo that empties the archive.",
					source.MaxStripComponents))
			}
			req.StripComponents = *p.StripComponents
		}

	case source.TypeGit:
		if p.StripComponents != nil {
			return nil, wrongSourceTypeError("source.strip_components", "tarball", "git")
		}
		if p.Ref != nil {
			if apiErr := validateGitRef(*p.Ref); apiErr != nil {
				return nil, apiErr
			}
			req.Ref = *p.Ref
		}
		if p.Depth != nil {
			if *p.Depth < 1 {
				return nil, invalidParamError("source.depth",
					"must be at least 1. A clone of no commits has no working tree to seed.")
			}
			req.Depth = *p.Depth
		}
		if p.CredentialRef != nil {
			if apiErr := validateCredentialRef(*p.CredentialRef); apiErr != nil {
				return nil, apiErr
			}
			req.CredentialRef = *p.CredentialRef
		}
	}
	return req, nil
}

// wrongSourceTypeError refuses a field that belongs to the other source type.
func wrongSourceTypeError(param, belongsTo, got string) *apiError {
	return invalidParamError(param, fmt.Sprintf(
		"belongs to a %s source, and this one is a %s. It is refused rather than ignored, "+
			"because a source that quietly disregards half of what was asked for seeds the wrong project.",
		belongsTo, got))
}

// validateGitRef refuses a ref that is not one.
//
// The value becomes an argument to git, so the leading dash is the one that
// matters: `--branch --upload-pack=...` would be a caller choosing what runs on
// this host if git ever read it as an option rather than as a value. The rest of
// the character set is git's own ref-name rule, kept deliberately narrow -- a ref
// this refuses is one no forge would have accepted either.
func validateGitRef(ref string) *apiError {
	notARef := func(why string) *apiError {
		return invalidParamError("source.ref", fmt.Sprintf(
			"%q is not a branch, tag or commit: %s", elideParam(ref), why))
	}

	switch {
	case ref == "":
		return notARef("it is empty. Leave it out for the remote's default branch.")
	case len(ref) > maxSourceRefBytes:
		return notARef(fmt.Sprintf("a ref may be at most %d bytes.", maxSourceRefBytes))
	case strings.HasPrefix(ref, "-"), strings.HasPrefix(ref, "."), strings.HasPrefix(ref, "/"):
		return notARef("it may not begin with \"-\", \".\" or \"/\".")
	case strings.Contains(ref, ".."):
		return notARef("it may not contain \"..\".")
	}
	for _, c := range ref {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.ContainsRune("._-+/@", c)
		if !ok {
			return notARef("it may hold only letters, digits and ._-+/@")
		}
	}
	return nil
}

// validateCredentialRef refuses a name that could not be one.
//
// The name is the operator's key, not a secret, and it is echoed back on the
// sandbox -- so it is held to a printable, boring character set. Whether the name
// exists is not decided here: that answer is source_not_permitted, and it comes
// from the core, because the difference between "no such credential" and "not
// allowlisted" is the operator's business.
func validateCredentialRef(ref string) *apiError {
	notARef := func(why string) *apiError {
		return invalidParamError("source.credential_ref", fmt.Sprintf(
			"%q is not a credential name: %s", elideParam(ref), why))
	}

	switch {
	case ref == "":
		return notARef("it is empty. Leave it out for a public repository.")
	case len(ref) > maxSourceCredentialRefBytes:
		return notARef(fmt.Sprintf("a name may be at most %d bytes.", maxSourceCredentialRefBytes))
	}
	for _, c := range ref {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.ContainsRune("._-", c)
		if !ok {
			return notARef("it may hold only letters, digits and ._-")
		}
	}
	return nil
}

// elideParam shortens a caller's value for an error message. A value refused for
// its length is by definition too long to quote back.
func elideParam(s string) string {
	const max = 48
	if len(s) <= max {
		return s
	}
	for i := range s {
		if i > max {
			return s[:i] + "..."
		}
	}
	return s
}

// sourceCreateError maps a create that failed while seeding onto the wire.
//
// Three outcomes, because three different people fix them: a refusal is the
// caller's URL against the operator's policy, an origin that misbehaved is
// somebody else's server, and a failed write is ours.
func sourceCreateError(err error) error {
	// The sandbox's own writable layer, which is smaller than the operator's caps.
	// A request error rather than a 502: nothing went wrong, the caller asked for a
	// sandbox that cannot hold what they asked to put in it, and the message names
	// the two fields that would.
	var tooLarge *sandbox.SeedTooLargeError
	if errors.As(err, &tooLarge) {
		return invalidParamError("source.url", fmt.Sprintf(
			"%s. Raise mem_mib or disk_mib, or seed a smaller source.", tooLarge))
	}

	// The seed took longer than the whole lifetime the caller asked for. Their
	// number, so a request error naming it -- and not the 500 a failed write would
	// otherwise become, for a condition nothing on this host did wrong.
	var expired *sandbox.SeedExpiredError
	if errors.As(err, &expired) {
		return invalidParamError("ttl_seconds", fmt.Sprintf(
			"%s. Raise it past how long the source takes to fetch and write.", expired))
	}

	// This node is already fetching as many sources as it will. Capacity, like a
	// full node, because the caller's move is the same and it is the type their SDK
	// already backs off on.
	var busy *sandbox.SeedBusyError
	if errors.As(err, &busy) {
		return capacityError(busy)
	}

	var seed *sandbox.SeedError
	if !errors.As(err, &seed) {
		return nil // not a seeding failure; the caller's own mapping applies
	}

	switch {
	case errors.Is(seed, source.ErrNotPermitted):
		return sourceNotPermittedError(seed)
	case errors.Is(seed, source.ErrFetchFailed),
		errors.Is(seed, source.ErrTooLarge),
		errors.Is(seed, source.ErrInvalidArchive):
		return sourceFetchFailedError(string(seed.Stage), seed.Err)
	default:
		// The write stage, with no failure of source's own inside it: the guest or
		// this host, and nothing the caller did.
		return internalError(seed)
	}
}

// toAPISource reports what a sandbox was seeded from.
func toAPISource(src *source.Result) *apitypes.SandboxSource {
	if src == nil {
		return nil
	}
	// The URL was redacted where it was fetched, not here. A field that arrives
	// already safe cannot be published unsafely by whoever reports it next.
	out := &apitypes.SandboxSource{
		Type:        apitypes.SandboxSourceType(src.Type),
		UrlRedacted: src.URLRedacted,
	}
	if src.Ref != "" {
		out.Ref = ptr(src.Ref)
	}
	if src.Commit != "" {
		out.Commit = ptr(src.Commit)
	}
	if src.CredentialRef != "" {
		out.CredentialRef = ptr(src.CredentialRef)
	}
	return out
}
