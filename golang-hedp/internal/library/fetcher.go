package library

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Fetcher retrieves a library bundle for a revision.
//
// This is the piece that makes horizontal scaling work. Behind a Kubernetes
// Service a pod cannot be *pushed* a version: a PUT lands on whichever replica
// the load balancer picked, and the other N-1 replicas never hear about it.
// Pods have to pull instead - every replica independently resolves a revision
// it does not have, from a source all of them can reach.
type Fetcher interface {
	// Fetch returns the bundle bytes for a revision, or an error. A revision
	// that genuinely does not exist must return ErrRevisionNotFound so the
	// caller can answer 404 instead of 503.
	Fetch(ctx context.Context, revision string) ([]byte, error)
}

// ErrRevisionNotFound distinguishes "this commit does not exist" from "the
// artifact store is down". The first is the caller's mistake, the second is
// ours, and they are different HTTP statuses.
var ErrRevisionNotFound = fmt.Errorf("revision not found")

// HTTPFetcher pulls bundles from a URL template, e.g.
//
//	https://artifacts.internal/hedp/{revision}.tar
//
// Any store that speaks HTTP works: an S3 or GCS bucket, an artifact service,
// an OCI registry behind a proxy, or a sidecar that shells out to git archive.
type HTTPFetcher struct {
	// URLTemplate must contain {revision}.
	URLTemplate string
	Client      *http.Client
	// MaxBytes caps a downloaded bundle.
	MaxBytes int64
}

// NewHTTPFetcher validates the template and returns a fetcher.
func NewHTTPFetcher(urlTemplate string, maxBytes int64) (*HTTPFetcher, error) {
	if !strings.Contains(urlTemplate, "{revision}") {
		return nil, fmt.Errorf("bundle URL template %q must contain {revision}", urlTemplate)
	}
	if maxBytes <= 0 {
		maxBytes = 512 << 20
	}
	return &HTTPFetcher{
		URLTemplate: urlTemplate,
		Client:      &http.Client{Timeout: 2 * time.Minute},
		MaxBytes:    maxBytes,
	}, nil
}

func (f *HTTPFetcher) Fetch(ctx context.Context, revision string) ([]byte, error) {
	url := strings.ReplaceAll(f.URLTemplate, "{revision}", revision)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden:
		// S3-style buckets answer 403 for absent objects when listing is
		// denied, so treat it as absence rather than as an outage.
		return nil, fmt.Errorf("%w: %s returned %s", ErrRevisionNotFound, url, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if int64(len(data)) > f.MaxBytes {
		return nil, fmt.Errorf("bundle at %s exceeds %d bytes", url, f.MaxBytes)
	}
	return data, nil
}

// GetOrFetch returns a resident version, pulling it if the registry has a
// Fetcher and does not already hold it.
//
// Concurrent misses for the same revision collapse into one fetch and one
// load, which matters more here than it does for a push: a burst of requests
// for a newly built commit arrives at every replica at once, and at ~134 MB
// and a few hundred milliseconds apiece, N simultaneous loads of the same
// bundle is how a pod OOMs.
func (r *Registry) GetOrFetch(ctx context.Context, revision string) (*Version, error) {
	if v, ok := r.Get(revision); ok {
		return v, nil
	}
	if r.fetcher == nil {
		return nil, fmt.Errorf("%w: revision %s is not resident and no bundle source is configured",
			ErrRevisionNotFound, revision)
	}

	// LoadBundle already deduplicates concurrent loads of a revision, but the
	// fetch happens before it, so the gate has to cover the download too.
	r.mu.Lock()
	if v, ok := r.versions[revision]; ok {
		v.lastUsed = time.Now()
		r.mu.Unlock()
		return v, nil
	}
	if gate, ok := r.fetching[revision]; ok {
		r.mu.Unlock()
		gate.done.Wait()
		return gate.ver, gate.err
	}
	gate := &loadGate{}
	gate.done.Add(1)
	r.fetching[revision] = gate
	r.mu.Unlock()

	bundle, err := r.fetcher.Fetch(ctx, revision)
	if err == nil {
		gate.ver, gate.err = r.LoadBundle(revision, bundle)
	} else {
		gate.err = err
	}

	r.mu.Lock()
	delete(r.fetching, revision)
	r.mu.Unlock()

	gate.done.Done()
	return gate.ver, gate.err
}
