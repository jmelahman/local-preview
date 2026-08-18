package store

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrNotInTier is returned by Hydrate when the durable tier has no object for
// the requested (repo, side, hash). It is distinct from "no tier configured"
// so a serve path can tell a reconcile gap (live deploy, artifact never
// persisted) from a plain single-node instance with no tier at all.
var ErrNotInTier = errors.New("artifact not present in durable tier")

// ArtifactTier is the durable, content-addressed artifact store the local
// on-disk cache is hydrated from and persisted to. Implemented by
// *s3store.Tier; an interface so tests can substitute a fake and so store
// stays free of any cloud dependency. side is "fe", "be", or "dl".
//
// It lives in store — not build — because the serving path
// (proxy → supervise.Manager) never passes through build yet must be able to
// bring an evicted artifact back on demand. build persists through the same
// interface.
type ArtifactTier interface {
	// Save uploads srcDir's contents under the (repo, side, hash) key,
	// idempotently.
	Save(ctx context.Context, repo, side, hash, srcDir string) error
	// Open returns a reader over the artifact's decompressed tar bytes, or
	// found=false when absent. Close verifies integrity.
	Open(ctx context.Context, repo, side, hash string) (rc io.ReadCloser, found bool, err error)
}

// SetArtifactTier attaches the durable tier. nil (the default) leaves local
// disk authoritative — Hydrate then only ever reports "already resident" or
// "no tier". Set once at startup, before any serve or build.
func (s *Store) SetArtifactTier(t ArtifactTier) { s.tier = t }

// ArtifactTier returns the configured durable tier, or nil. Callers that
// persist (build) or report durable usage read it here rather than holding
// their own copy, so there is a single source of truth.
func (s *Store) ArtifactTier() ArtifactTier { return s.tier }

// Hydrate makes an artifact side resident on local disk by fetching it from
// the durable tier and publishing it through the same atomic rename a build
// uses. It is the inverse of eviction: local disk is a cache, and this refills
// it.
//
// Returns nil when the side is already resident or was successfully hydrated.
// Returns an error otherwise: no tier configured, the tier lacks the object
// (ErrNotInTier), or extraction/verification/publish failed. On the build path
// the caller ignores the error and rebuilds; on the serve path it distinguishes
// "not resident, hydrate and serve" from "genuinely gone".
//
// Concurrent hydrates of the same content-address are deduplicated with
// singleflight, so two simultaneous serve requests fetch the object once.
func (s *Store) Hydrate(ctx context.Context, repo, side, hash string) error {
	if hash == "" {
		return fmt.Errorf("hydrate %s/%s: empty hash", repo, side)
	}
	if s.hasSide(repo, side, hash) {
		return nil
	}
	if s.tier == nil {
		return fmt.Errorf("hydrate %s %s/%s: no durable tier configured", repo, side, hash)
	}
	key := repo + ":hydrate:" + side + ":" + hash
	_, err, _ := s.sf.Do(key, func() (any, error) {
		// A sibling may have landed it while we waited on the singleflight.
		if s.hasSide(repo, side, hash) {
			return nil, nil
		}
		rc, found, err := s.tier.Open(ctx, repo, side, hash)
		if err != nil {
			return nil, fmt.Errorf("hydrate %s %s/%s: open: %w", repo, side, hash, err)
		}
		if !found {
			return nil, fmt.Errorf("hydrate %s %s/%s: %w", repo, side, hash, ErrNotInTier)
		}
		dir, cleanup, err := s.NewScratchDir("hydrate")
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("hydrate %s %s/%s: scratch: %w", repo, side, hash, err)
		}
		defer cleanup()
		extractErr := ExtractTar(rc, dir)
		// Close both releases the connection and verifies the decompressed
		// length against the size recorded at Save time — a truncated object
		// must never land under a content-addressed key.
		closeErr := rc.Close()
		if extractErr != nil {
			return nil, fmt.Errorf("hydrate %s %s/%s: extract: %w", repo, side, hash, extractErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("hydrate %s %s/%s: verify: %w", repo, side, hash, closeErr)
		}
		if err := s.publishSide(repo, side, hash, dir); err != nil {
			return nil, fmt.Errorf("hydrate %s %s/%s: publish: %w", repo, side, hash, err)
		}
		s.NoteAccess(repo, side, hash)
		return nil, nil
	})
	return err
}

// hasSide reports whether an artifact side is resident on local disk.
func (s *Store) hasSide(repo, side, hash string) bool {
	switch side {
	case "fe":
		return s.HasFrontend(repo, hash)
	case "be":
		return s.HasBackend(repo, hash)
	case "dl":
		return s.HasArtifact(repo, hash)
	}
	return false
}

// publishSide lands a hydrated side's extracted tree via the same atomic
// publish a build uses. overwrite is false: hydrate only fills a missing
// artifact, never displaces a resident one.
func (s *Store) publishSide(repo, side, hash, srcDir string) error {
	switch side {
	case "fe":
		return s.PublishFrontend(repo, hash, srcDir, false)
	case "be":
		return s.PublishBackend(repo, hash, srcDir, false)
	case "dl":
		return s.PublishArtifactDir(repo, hash, srcDir, false)
	}
	return fmt.Errorf("hydrate: unknown side %q", side)
}
