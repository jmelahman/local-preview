// Package watch polls watched repos' mirror clones and deploys new branch
// tips — the trigger for repos that push somewhere the server can fetch
// from but can't run a post-commit hook or deliver a webhook. It is a thin
// adapter over the build queue: fetch, list branch heads, request a deploy
// for any matching tip that has none.
package watch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
)

// DefaultInterval is how often watched repos are polled unless configured
// otherwise.
const DefaultInterval = time.Minute

// Watcher periodically fetches every watched repo and deploys branch tips
// that have no deploy yet.
type Watcher struct {
	db       *db.Store
	git      *gitrepo.Manager
	queue    *build.Queue
	interval time.Duration
	kick     chan struct{}
}

// New returns a Watcher polling at interval. A non-positive interval
// disables it (Start and Kick become no-ops).
func New(database *db.Store, git *gitrepo.Manager, queue *build.Queue, interval time.Duration) *Watcher {
	return &Watcher{
		db:       database,
		git:      git,
		queue:    queue,
		interval: interval,
		kick:     make(chan struct{}, 1),
	}
}

// Start launches the poll loop in a goroutine; it polls once immediately,
// then on every interval tick (or sooner after a Kick).
func (w *Watcher) Start(ctx context.Context) {
	if w.interval <= 0 {
		return
	}
	go func() {
		w.PollAll(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-w.kick:
			}
			w.PollAll(ctx)
		}
	}()
}

// Kick requests an out-of-band poll — called after watch settings change so
// a newly watched repo doesn't wait a full interval. Never blocks.
func (w *Watcher) Kick() {
	if w == nil || w.interval <= 0 {
		return
	}
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// PollAll polls every watched repo once. Per-repo failures are logged, not
// returned: one unreachable origin must not stall the others.
func (w *Watcher) PollAll(ctx context.Context) {
	repos, err := w.db.ListRepos()
	if err != nil {
		log.Printf("watch: list repos: %v", err)
		return
	}
	for _, repo := range repos {
		if !repo.Watch || ctx.Err() != nil {
			continue
		}
		if err := w.pollRepo(ctx, repo); err != nil && ctx.Err() == nil {
			log.Printf("watch: %s: %v", repo.Name, err)
		}
	}
}

// pollRepo fetches one repo, requests a deploy for every matching branch tip
// that doesn't have one, then evicts deploys whose commit fell out of the
// repo's branch history. Tips are deployed by sha, so RequestDeploy resolves
// them without a second fetch.
func (w *Watcher) pollRepo(ctx context.Context, repo db.Repo) error {
	gr := w.git.Open(repo.Name)
	if err := gr.Fetch(ctx); err != nil {
		return err
	}
	tips, err := gr.BranchTips(ctx)
	if err != nil {
		return err
	}
	patterns := SplitPatterns(repo.WatchBranches)
	for _, tip := range tips {
		if !MatchBranch(patterns, tip.Branch) {
			continue
		}
		if _, err := w.db.GetDeployBySHA(repo.ID, tip.SHA); err == nil {
			continue
		} else if !errors.Is(err, db.ErrNotFound) {
			return err
		}
		if _, err := w.queue.RequestDeploy(ctx, repo.Name, tip.SHA, false); err != nil {
			log.Printf("watch: deploy %s@%s: %v", repo.Name, tip.Branch, err)
			continue
		}
		log.Printf("watch: deploying %s@%s (%.7s)", repo.Name, tip.Branch, tip.SHA)
	}
	// The fetch above prunes deleted branches. Reachability is judged against
	// every surviving tip — not just the watched ones — so a commit still
	// living on an unwatched branch keeps its preview.
	tipSHAs := make([]string, len(tips))
	for i, tip := range tips {
		tipSHAs[i] = tip.SHA
	}
	n, err := w.queue.EvictUnreachable(ctx, repo, tipSHAs)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("watch: %s: evicted %d deploy(s) for deleted branches", repo.Name, n)
	}
	return nil
}

// SplitPatterns splits a comma-separated branch pattern list, dropping
// empty entries.
func SplitPatterns(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ValidatePatterns canonicalizes a comma-separated branch pattern list
// (path.Match globs; `*` doesn't cross `/`). Empty input is valid and means
// "all branches".
func ValidatePatterns(s string) (string, error) {
	parts := SplitPatterns(s)
	for _, p := range parts {
		if _, err := path.Match(p, "x"); err != nil {
			return "", fmt.Errorf("invalid branch pattern %q", p)
		}
	}
	return strings.Join(parts, ","), nil
}

// MatchBranch reports whether branch matches any pattern. An empty pattern
// list matches every branch.
func MatchBranch(patterns []string, branch string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if ok, _ := path.Match(p, branch); ok {
			return true
		}
	}
	return false
}
