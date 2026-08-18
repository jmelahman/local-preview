package build

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jmelahman/local-preview/internal/s3store"
	"github.com/jmelahman/local-preview/internal/store"
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
// disables persist and hydrate. Call before Start. The tier is owned by the
// store — the serving path hydrates through it without passing through build —
// so this also hands it to the store; the queue keeps a copy only for the
// persist pool's hot-path nil checks.
func (q *Queue) SetArtifactTier(t store.ArtifactTier) {
	q.tier = t
	q.files.SetArtifactTier(t)
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
// durable tier, so a build can be skipped. No-op when no tier is configured.
// Best-effort on the build path: a failure (including the tier simply not
// having the object) falls through to a rebuild, so only genuine errors —
// not a plain miss — are logged. The heavy lifting, singleflighted against the
// same fetch the serving path uses, lives in the store.
func (q *Queue) hydrate(ctx context.Context, repo, side, hash string) {
	if q.tier == nil {
		return
	}
	if err := q.files.Hydrate(ctx, repo, side, hash); err != nil && !errors.Is(err, store.ErrNotInTier) {
		log.Printf("build: %v", err)
	}
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
