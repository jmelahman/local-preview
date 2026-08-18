package build

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jmelahman/local-preview/internal/s3store"
)

const (
	// persistWorkers upload artifacts to the durable tier concurrently.
	persistWorkers = 4
	// persistQueueSize bounds buffered uploads; a full queue drops the oldest
	// enqueue (best-effort — a reconcile pass is the correctness net).
	persistQueueSize = 256
	// persistTimeout bounds a single artifact upload.
	persistTimeout = 10 * time.Minute
)

// persistJob names a published artifact side to upload. The source directory
// is resolved from the store at upload time (the stable published path), not
// carried here.
type persistJob struct {
	repo string
	side string // "fe", "be", "dl"
	hash string
}

// SetArtifactTier attaches the durable artifact tier. nil (the default)
// disables persist and hydrate. Call before Start.
func (q *Queue) SetArtifactTier(t ArtifactTier) {
	q.tier = t
}

// startPersist launches the upload worker pool when a tier is configured.
func (q *Queue) startPersist() {
	if q.tier == nil {
		return
	}
	q.persistJobs = make(chan persistJob, persistQueueSize)
	q.persistQuit = make(chan struct{})
	for range persistWorkers {
		q.persistWG.Add(1)
		go q.persistWorker()
	}
}

// Stop drains in-flight and buffered artifact uploads, then returns. Call it
// during graceful shutdown, after the build workers' context is cancelled so
// no new uploads are being enqueued. Safe to call when no tier is configured.
func (q *Queue) Stop() {
	if q.persistQuit == nil {
		return
	}
	close(q.persistQuit)
	q.persistWG.Wait()
}

func (q *Queue) persistWorker() {
	defer q.persistWG.Done()
	for {
		select {
		case job := <-q.persistJobs:
			q.runPersist(job)
		case <-q.persistQuit:
			// Drain what's buffered so a normal shutdown doesn't drop a
			// just-built artifact, then exit.
			for {
				select {
				case job := <-q.persistJobs:
					q.runPersist(job)
				default:
					return
				}
			}
		}
	}
}

// enqueuePersist schedules an upload of a published artifact side. No-op when
// no tier is configured. Non-blocking: a full queue logs and drops (the upload
// will be recovered by a rebuild, or a future reconcile pass).
func (q *Queue) enqueuePersist(repo, side, hash string) {
	if q.tier == nil || q.persistJobs == nil || hash == "" {
		return
	}
	select {
	case q.persistJobs <- persistJob{repo: repo, side: side, hash: hash}:
	default:
		log.Printf("build: persist queue full, dropping %s %s/%s", repo, side, hash)
	}
}

// runPersist uploads one artifact side. It uses a fresh timeout context, not
// the build context, so a build finishing (or the server draining) doesn't
// abort an in-flight upload mid-stream. A source directory that GC deleted
// out from under the upload is a benign drop, not an error.
func (q *Queue) runPersist(job persistJob) {
	dir := q.artifactDir(job.repo, job.side, job.hash)
	if dir == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if err := q.tier.Save(ctx, job.repo, job.side, job.hash, dir); err != nil {
		if errors.Is(err, s3store.ErrSourceGone) {
			return // retention won the race; nothing to persist
		}
		log.Printf("build: persist %s %s/%s: %v", job.repo, job.side, job.hash, err)
	}
}

// hydrate makes an artifact side present locally by fetching it from the
// durable tier, so a build can be skipped. No-op when no tier is configured or
// the side is already present. Best-effort: any failure logs and falls through
// to a rebuild. Concurrent hydrates of the same content-address are
// deduplicated with singleflight (as builds are).
func (q *Queue) hydrate(ctx context.Context, repo, side, hash string) {
	if q.tier == nil || hash == "" || q.hasSide(repo, side, hash) {
		return
	}
	key := repo + ":hydrate:" + side + ":" + hash
	q.sf.Do(key, func() (any, error) {
		// A sibling may have landed it while we waited on the singleflight.
		if q.hasSide(repo, side, hash) {
			return nil, nil
		}
		rc, found, err := q.tier.Open(ctx, repo, side, hash)
		if err != nil {
			log.Printf("build: hydrate %s %s/%s: %v", repo, side, hash, err)
			return nil, nil
		}
		if !found {
			return nil, nil
		}
		dir, cleanup, err := q.files.NewScratchDir("hydrate")
		if err != nil {
			rc.Close()
			log.Printf("build: hydrate %s %s/%s: scratch: %v", repo, side, hash, err)
			return nil, nil
		}
		defer cleanup()
		extractErr := extractTar(rc, dir)
		// Close both releases the connection and verifies the decompressed
		// length against the size recorded at Save time.
		closeErr := rc.Close()
		if extractErr != nil {
			log.Printf("build: hydrate %s %s/%s: extract: %v", repo, side, hash, extractErr)
			return nil, nil
		}
		if closeErr != nil {
			log.Printf("build: hydrate %s %s/%s: %v", repo, side, hash, closeErr)
			return nil, nil
		}
		if err := q.publishSide(repo, side, hash, dir); err != nil {
			log.Printf("build: hydrate %s %s/%s: publish: %v", repo, side, hash, err)
			return nil, nil
		}
		log.Printf("build: hydrated %s %s/%s from durable tier", repo, side, hash)
		return nil, nil
	})
}

func (q *Queue) hasSide(repo, side, hash string) bool {
	switch side {
	case "fe":
		return q.files.HasFrontend(repo, hash)
	case "be":
		return q.files.HasBackend(repo, hash)
	case "dl":
		return q.files.HasArtifact(repo, hash)
	}
	return false
}

func (q *Queue) artifactDir(repo, side, hash string) string {
	switch side {
	case "fe":
		return q.files.FrontendDir(repo, hash)
	case "be":
		return q.files.BackendDir(repo, hash)
	case "dl":
		return q.files.ArtifactDir(repo, hash)
	}
	return ""
}

// publishSide lands a hydrated side's extracted tree into the store via the
// same atomic publish a build uses. overwrite is false: hydrate only fills a
// missing artifact, never displaces a present one.
func (q *Queue) publishSide(repo, side, hash, srcDir string) error {
	switch side {
	case "fe":
		return q.files.PublishFrontend(repo, hash, srcDir, false)
	case "be":
		return q.files.PublishBackend(repo, hash, srcDir, false)
	case "dl":
		return q.files.PublishArtifactDir(repo, hash, srcDir, false)
	}
	return nil
}
