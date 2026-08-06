// Package gitdiff turns a Git revision range into a list of changed files.
//
// It deliberately never looks at unified diff text (the "@@ -1,7 +1,9 @@"
// hunk format). Unified diff is a line-oriented rendering meant for humans;
// reconstructing structured data from it is lossy and brittle. Instead this
// package asks Git two much simpler questions:
//
//  1. Which paths changed, and how? (git diff --name-status -z)
//  2. What are the full bytes of a path at a revision? (git cat-file blob)
//
// Both answers are exact and machine-readable, so everything downstream can
// work on whole documents rather than on patch fragments.
package gitdiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Status is a Git change status as reported by --name-status.
type Status string

const (
	Added       Status = "A"
	Modified    Status = "M"
	Deleted     Status = "D"
	Renamed     Status = "R"
	Copied      Status = "C"
	TypeChanged Status = "T"
)

// Change is a single changed path in a revision range.
type Change struct {
	Status Status `json:"status"`
	// Path is the path at head. For Deleted it is the path at base.
	Path string `json:"path"`
	// OldPath is set for Renamed and Copied only.
	OldPath string `json:"old_path,omitempty"`
	// Score is the rename/copy similarity percentage, 0 otherwise.
	Score int `json:"score,omitempty"`
}

// ErrNotFound reports that a path does not exist at a revision.
var ErrNotFound = errors.New("gitdiff: path not found at revision")

// Repo is a checkout to run Git commands against.
type Repo struct {
	// Dir is any directory inside the working tree.
	Dir string
	// Git overrides the git binary. Empty means "git" from PATH.
	Git string
}

// Blob is the content of one path at one revision.
type Blob struct {
	// ID is the Git object ID. It is a content hash, which makes it a
	// ready-made version token for optimistic concurrency on the server.
	ID   string
	Data []byte
}

// Options controls how the range is interpreted.
type Options struct {
	// MergeBase compares head against the merge base of base and head
	// (Git's "base...head"). This is what a merge-request pipeline almost
	// always wants: only what this branch introduced, ignoring commits that
	// landed on the target branch in the meantime. Without it, unrelated
	// target-branch work shows up as changes to revert.
	MergeBase bool
	// DetectRenames enables Git's rename detection (-M).
	DetectRenames bool
	// Pathspecs limits the diff to these pathspecs, e.g. "config/".
	Pathspecs []string
}

// Changes lists the paths that differ between base and head.
func (r Repo) Changes(ctx context.Context, base, head string, opt Options) ([]Change, error) {
	rng := base + ".." + head
	if opt.MergeBase {
		rng = base + "..." + head
	}
	args := []string{"diff", "--name-status", "-z"}
	if opt.DetectRenames {
		args = append(args, "-M")
	} else {
		args = append(args, "--no-renames")
	}
	args = append(args, rng)
	if len(opt.Pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, opt.Pathspecs...)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseNameStatusZ(out)
}

// Blob returns the content of path at rev. It reports ErrNotFound when the
// path does not exist there, which is the normal case for the base side of an
// added file and the head side of a deleted one.
func (r Repo) Blob(ctx context.Context, rev, path string) (Blob, error) {
	id, err := r.objectID(ctx, rev, path)
	if err != nil {
		return Blob{}, err
	}
	data, err := r.run(ctx, "cat-file", "blob", id)
	if err != nil {
		return Blob{}, err
	}
	return Blob{ID: id, Data: data}, nil
}

// objectID resolves <rev>:<path> to a blob ID without reading the content.
func (r Repo) objectID(ctx context.Context, rev, path string) (string, error) {
	// --verify -q exits 1 with no stderr when the object does not exist,
	// which is how we tell "missing" apart from "git is broken".
	out, err := r.run(ctx, "rev-parse", "--verify", "-q", rev+":"+path)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", fmt.Errorf("%w: %s:%s", ErrNotFound, rev, path)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	bin := r.Git
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", r.Dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// parseNameStatusZ decodes the NUL-separated --name-status stream.
//
// The -z form matters: without it Git quotes and escapes paths containing
// spaces, quotes or non-ASCII bytes, and every hand-rolled parser gets that
// wrong eventually. With -z the fields are raw bytes with NUL separators.
//
// Records are "status\0path\0", except renames and copies which carry both
// sides: "R096\0old\0new\0".
func parseNameStatusZ(out []byte) ([]Change, error) {
	fields := strings.Split(string(out), "\x00")
	var changes []Change
	for i := 0; i < len(fields); {
		f := fields[i]
		if f == "" {
			i++
			continue
		}
		code := Status(f[:1])
		score := 0
		if len(f) > 1 {
			n, err := strconv.Atoi(f[1:])
			if err != nil {
				return nil, fmt.Errorf("gitdiff: unrecognised status field %q", f)
			}
			score = n
		}
		switch code {
		case Renamed, Copied:
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("gitdiff: truncated %s record", code)
			}
			changes = append(changes, Change{
				Status:  code,
				OldPath: fields[i+1],
				Path:    fields[i+2],
				Score:   score,
			})
			i += 3
		case Added, Modified, Deleted, TypeChanged:
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("gitdiff: truncated %s record", code)
			}
			changes = append(changes, Change{Status: code, Path: fields[i+1], Score: score})
			i += 2
		default:
			// "U" (unmerged) and "X" (bug in git) have no sane mapping to an
			// API call. Fail loudly rather than sync half a conflict.
			return nil, fmt.Errorf("gitdiff: unsupported status %q for %q", f, fields[min(i+1, len(fields)-1)])
		}
	}
	return changes, nil
}
