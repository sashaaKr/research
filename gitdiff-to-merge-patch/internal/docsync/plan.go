// Package docsync turns a list of changed files into a list of HTTP requests.
//
// The central design choice is that a patch is never trusted just because a
// library produced it. Every generated patch is applied locally to the base
// document and the result compared against the head document. A patch that
// does not reproduce head exactly is discarded and a fallback is used. That
// single check is what makes the pipeline safe: the failure mode of a wrong
// patch stops being "the server quietly holds different data than Git" and
// becomes "this file was sent as a full replacement instead".
package docsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/wI2L/jsondiff"

	"github.com/sashaakr/research/gitdiff-to-merge-patch/internal/gitdiff"
)

// Media types for the two IETF patch formats.
const (
	MediaTypeJSON       = "application/json"
	MediaTypeMergePatch = "application/merge-patch+json" // RFC 7396
	MediaTypeJSONPatch  = "application/json-patch+json"  // RFC 6902
)

// BaseVersionHeader carries the Git blob ID of the document the patch was
// computed against, so the server can refuse to apply a patch to a document
// it does not recognise. See the README section on concurrency.
const BaseVersionHeader = "X-Base-Version"

// Strategy records how a request body was produced.
type Strategy string

const (
	StrategyMergePatch Strategy = "merge-patch" // RFC 7396 PATCH
	StrategyJSONPatch  Strategy = "json-patch"  // RFC 6902 PATCH
	StrategyReplace    Strategy = "replace"     // full-document PUT
	StrategyNone       Strategy = "none"        // DELETE, no body
)

// Op is the intent of a request, independent of HTTP method.
type Op string

const (
	OpCreate Op = "create"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
)

// Fallback selects what to do when a merge patch cannot faithfully express a
// change. See RFC 7396 section 1: a merge patch cannot set a member to null,
// because null is the deletion sentinel.
type Fallback string

const (
	// FallbackReplace sends the whole head document with PUT. Always correct,
	// costs bandwidth, and needs an idempotent PUT on the server.
	FallbackReplace Fallback = "replace"
	// FallbackJSONPatch emits an RFC 6902 patch instead, which has no null
	// ambiguity. Requires the server to support that media type.
	FallbackJSONPatch Fallback = "json-patch"
	// FallbackError refuses to plan the change at all.
	FallbackError Fallback = "error"
)

// Request is one HTTP call in the sync plan.
type Request struct {
	Op       Op                `json:"op"`
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     json.RawMessage   `json:"body,omitempty"`
	Strategy Strategy          `json:"strategy"`
	// Reason explains a non-default strategy, e.g. why a merge patch was
	// rejected. Empty on the happy path.
	Reason string         `json:"reason,omitempty"`
	Change gitdiff.Change `json:"change"`
}

// Planner converts Git changes into requests.
type Planner struct {
	// Resolve maps a repository path to the resource URL that owns it.
	// Returning ok == false skips the file. Required.
	Resolve func(repoPath string) (url string, ok bool)

	// Canonical normalises a file's bytes to canonical JSON.
	// Defaults to the package-level Canonical.
	Canonical func(name string, data []byte) ([]byte, error)

	// Fallback selects the strategy when a merge patch is not faithful.
	// The zero value is FallbackReplace.
	Fallback Fallback

	// IncludeBaseVersion attaches X-Base-Version to update and delete
	// requests.
	IncludeBaseVersion bool
}

// Plan builds the request list for a revision range.
//
// Requests are emitted in the order Git reported the changes, except that a
// rename across resource boundaries emits its DELETE before its PUT.
func (p Planner) Plan(ctx context.Context, repo gitdiff.Repo, base, head string, changes []gitdiff.Change) ([]Request, error) {
	if p.Resolve == nil {
		return nil, errors.New("docsync: Planner.Resolve is required")
	}
	var out []Request
	for _, ch := range changes {
		reqs, err := p.planOne(ctx, repo, base, head, ch)
		if err != nil {
			return nil, fmt.Errorf("docsync: %s %s: %w", ch.Status, ch.Path, err)
		}
		out = append(out, reqs...)
	}
	return out, nil
}

func (p Planner) planOne(ctx context.Context, repo gitdiff.Repo, base, head string, ch gitdiff.Change) ([]Request, error) {
	switch ch.Status {
	case gitdiff.Added:
		return p.planCreate(ctx, repo, head, ch)

	case gitdiff.Deleted:
		return p.planDelete(ctx, repo, base, ch, ch.Path)

	case gitdiff.Modified, gitdiff.TypeChanged:
		return p.planUpdate(ctx, repo, base, head, ch, ch.Path, ch.Path)

	case gitdiff.Copied:
		// A copy creates a new resource and leaves the original alone.
		return p.planCreate(ctx, repo, head, ch)

	case gitdiff.Renamed:
		oldURL, oldOK := p.Resolve(ch.OldPath)
		newURL, newOK := p.Resolve(ch.Path)
		switch {
		case oldOK && newOK && oldURL == newURL:
			// The resource identity lives inside the document, not in the
			// filename, so the move is invisible to the API: patch in place.
			return p.planUpdate(ctx, repo, base, head, ch, ch.OldPath, ch.Path)
		case oldOK && newOK:
			// The filename is the resource identity. A patch cannot move a
			// resource, so this is a delete followed by a create. Ordering
			// matters if the server enforces uniqueness on document fields.
			del, err := p.planDelete(ctx, repo, base, ch, ch.OldPath)
			if err != nil {
				return nil, err
			}
			add, err := p.planCreate(ctx, repo, head, ch)
			if err != nil {
				return nil, err
			}
			return append(del, add...), nil
		case oldOK:
			// Moved out of the synced tree: the resource is gone.
			return p.planDelete(ctx, repo, base, ch, ch.OldPath)
		case newOK:
			// Moved into the synced tree: the resource is new.
			return p.planCreate(ctx, repo, head, ch)
		default:
			return nil, nil
		}

	default:
		return nil, fmt.Errorf("unhandled status %q", ch.Status)
	}
}

func (p Planner) planCreate(ctx context.Context, repo gitdiff.Repo, head string, ch gitdiff.Change) ([]Request, error) {
	url, ok := p.Resolve(ch.Path)
	if !ok {
		return nil, nil
	}
	blob, err := repo.Blob(ctx, head, ch.Path)
	if err != nil {
		return nil, err
	}
	doc, err := p.canonical(ch.Path, blob.Data)
	if err != nil {
		return nil, err
	}
	return []Request{{
		Op:       OpCreate,
		Method:   http.MethodPut,
		URL:      url,
		Headers:  map[string]string{"Content-Type": MediaTypeJSON},
		Body:     json.RawMessage(doc),
		Strategy: StrategyReplace,
		Change:   ch,
	}}, nil
}

func (p Planner) planDelete(ctx context.Context, repo gitdiff.Repo, base string, ch gitdiff.Change, repoPath string) ([]Request, error) {
	url, ok := p.Resolve(repoPath)
	if !ok {
		return nil, nil
	}
	headers := map[string]string{}
	if p.IncludeBaseVersion {
		blob, err := repo.Blob(ctx, base, repoPath)
		if err != nil {
			return nil, err
		}
		headers[BaseVersionHeader] = blob.ID
	}
	if len(headers) == 0 {
		headers = nil
	}
	return []Request{{
		Op:       OpDelete,
		Method:   http.MethodDelete,
		URL:      url,
		Headers:  headers,
		Strategy: StrategyNone,
		Change:   ch,
	}}, nil
}

func (p Planner) planUpdate(ctx context.Context, repo gitdiff.Repo, base, head string, ch gitdiff.Change, basePath, headPath string) ([]Request, error) {
	url, ok := p.Resolve(headPath)
	if !ok {
		return nil, nil
	}
	baseBlob, err := repo.Blob(ctx, base, basePath)
	if err != nil {
		return nil, err
	}
	headBlob, err := repo.Blob(ctx, head, headPath)
	if err != nil {
		return nil, err
	}
	original, err := p.canonical(basePath, baseBlob.Data)
	if err != nil {
		return nil, err
	}
	target, err := p.canonical(headPath, headBlob.Data)
	if err != nil {
		return nil, err
	}

	// Git says the bytes changed; canonical JSON says the meaning did not.
	// Reformatting, key reordering and YAML comment edits all land here and
	// are dropped rather than sent.
	if jsonpatch.Equal(original, target) {
		return nil, nil
	}

	body, strategy, reason, err := p.buildPatch(original, target)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Content-Type": mediaTypeFor(strategy)}
	if p.IncludeBaseVersion {
		headers[BaseVersionHeader] = baseBlob.ID
	}
	method := http.MethodPatch
	if strategy == StrategyReplace {
		method = http.MethodPut
	}
	return []Request{{
		Op:       OpUpdate,
		Method:   method,
		URL:      url,
		Headers:  headers,
		Body:     json.RawMessage(body),
		Strategy: strategy,
		Reason:   reason,
		Change:   ch,
	}}, nil
}

// buildPatch produces the smallest faithful body for original -> target.
//
// "Faithful" is verified, not assumed: the candidate patch is applied to
// original and the result compared to target. Merge patch is tried first
// because it is the format the API speaks natively; the fallback only runs
// when the verification fails.
func (p Planner) buildPatch(original, target []byte) (body []byte, strategy Strategy, reason string, err error) {
	patch, mergeErr := jsonpatch.CreateMergePatch(original, target)
	if mergeErr == nil {
		ok, verifyErr := mergePatchIsFaithful(original, patch, target)
		if ok {
			return patch, StrategyMergePatch, "", nil
		}
		reason = explainMergeFailure(target, verifyErr)
	} else {
		reason = fmt.Sprintf("merge patch could not be generated: %v", mergeErr)
	}

	switch p.fallback() {
	case FallbackReplace:
		return target, StrategyReplace, reason, nil

	case FallbackJSONPatch:
		ops, err := jsondiff.CompareJSON(original, target)
		if err != nil {
			return nil, "", "", fmt.Errorf("%s; json patch fallback failed: %w", reason, err)
		}
		encoded, err := json.Marshal(ops)
		if err != nil {
			return nil, "", "", fmt.Errorf("%s; encoding json patch failed: %w", reason, err)
		}
		if ok, err := jsonPatchIsFaithful(original, encoded, target); !ok {
			return nil, "", "", fmt.Errorf("%s; json patch fallback also failed verification: %v", reason, err)
		}
		return encoded, StrategyJSONPatch, reason, nil

	default: // FallbackError
		return nil, "", "", errors.New(reason)
	}
}

// mergePatchIsFaithful applies patch to original and reports whether the
// result equals target.
func mergePatchIsFaithful(original, patch, target []byte) (bool, error) {
	got, err := jsonpatch.MergePatch(original, patch)
	if err != nil {
		return false, err
	}
	if !jsonpatch.Equal(got, target) {
		return false, nil
	}
	return true, nil
}

// jsonPatchIsFaithful runs the same verification for an RFC 6902 patch.
func jsonPatchIsFaithful(original, patch, target []byte) (bool, error) {
	decoded, err := jsonpatch.DecodePatch(patch)
	if err != nil {
		return false, err
	}
	got, err := decoded.Apply(original)
	if err != nil {
		return false, err
	}
	if !jsonpatch.Equal(got, target) {
		return false, nil
	}
	return true, nil
}

// explainMergeFailure turns a failed verification into a message an operator
// can act on. The overwhelmingly common cause is an explicit null in the
// target, which RFC 7396 reserves as the deletion sentinel.
func explainMergeFailure(target []byte, verifyErr error) string {
	if verifyErr != nil {
		return fmt.Sprintf("merge patch failed verification: %v", verifyErr)
	}
	if paths := explicitNullPaths(target); len(paths) > 0 {
		return fmt.Sprintf(
			"merge patch cannot express an explicit null (RFC 7396 reserves null for deletion); null at %v",
			paths,
		)
	}
	return "merge patch did not reproduce the target document"
}

// explicitNullPaths lists JSON Pointer paths where the document holds a
// literal null. Arrays are not descended into: merge patch replaces arrays
// wholesale, so a null inside one is carried across verbatim and harmless.
func explicitNullPaths(doc []byte) []string {
	var v any
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	var paths []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		for k, val := range obj {
			p := prefix + "/" + escapePointer(k)
			if val == nil {
				paths = append(paths, p)
				continue
			}
			walk(p, val)
		}
	}
	walk("", v)
	return paths
}

// escapePointer applies the RFC 6901 token escaping rules.
func escapePointer(tok string) string {
	out := make([]rune, 0, len(tok))
	for _, r := range tok {
		switch r {
		case '~':
			out = append(out, '~', '0')
		case '/':
			out = append(out, '~', '1')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

func (p Planner) canonical(name string, data []byte) ([]byte, error) {
	if p.Canonical != nil {
		return p.Canonical(name, data)
	}
	return Canonical(name, data)
}

func (p Planner) fallback() Fallback {
	if p.Fallback == "" {
		return FallbackReplace
	}
	return p.Fallback
}

func mediaTypeFor(s Strategy) string {
	switch s {
	case StrategyMergePatch:
		return MediaTypeMergePatch
	case StrategyJSONPatch:
		return MediaTypeJSONPatch
	default:
		return MediaTypeJSON
	}
}
