// Package fleet is the control node's view of the elastic worker tier: a
// heartbeat-tracked registry plus placement. It implements proxy.Backends by
// choosing a worker for a supervise.Key and delegating to that worker's
// transport — so the proxy routes to a fleet exactly as it routes to a single
// local Manager, the same "one orchestrator, two transports" property extended
// to N workers.
//
// Placement is rendezvous (highest-random-weight) hashing on (repo, hash):
// cache-affine (the same artifact lands on the same worker, so its local cache
// and any warm process are reused) and minimally disruptive when workers join
// or leave (only the keys whose top-weighted worker changed move). A
// process-mode frontend is co-placed with its backend — it hashes on the
// backend's hash — because the pair shares a per-deploy docker network that
// only exists on one node. A worker that is draining or at capacity is skipped
// for new placements, falling back to the least-loaded fresh worker.
package fleet

import (
	"context"
	"errors"
	"hash/fnv"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmelahman/local-preview/internal/supervise"
	"github.com/jmelahman/local-preview/internal/workerapi"
)

// ErrNoWorker means no worker could take the placement (none registered, all
// stale, or all draining). The proxy renders it like any other start failure.
var ErrNoWorker = errors.New("no worker available")

// Backend is one worker's control-side transport. *workerapi.Client satisfies
// it; tests substitute a fake.
type Backend interface {
	EnsureRunning(ctx context.Context, k supervise.Key, repoName string) (string, error)
	Heartbeat(ctx context.Context) (workerapi.Heartbeat, error)
	Report(ctx context.Context) ([]supervise.ProcReport, error)
	RunLog(ctx context.Context, repo, side, hash string, attempt int, offset int64) (supervise.RunLog, error)
	Stop(ctx context.Context, k supervise.Key, reason string) error
	Configure(ctx context.Context, cfg workerapi.WorkerConfig) error
}

type workerState struct {
	id string
	be Backend

	mu        sync.Mutex
	hb        workerapi.Heartbeat
	lastBeat  time.Time
	reachable bool
}

func (w *workerState) snapshot() (hb workerapi.Heartbeat, lastBeat time.Time, reachable bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hb, w.lastBeat, w.reachable
}

// Registry tracks workers and places keys onto them.
type Registry struct {
	mu         sync.RWMutex
	workers    map[string]*workerState
	staleAfter time.Duration

	// Merged fleet report cache — see runtime.go.
	reportMu    sync.Mutex
	reportAt    time.Time
	reportCache map[supervise.Key]procEntry

	// The dashboard-configured runtime settings the heartbeat loop reconciles
	// onto every worker (a rebooted worker comes back with its boot flags
	// until the next poll pushes these). -1 = unset.
	desiredMaxWarm atomic.Int64
	desiredMinWarm atomic.Int64
	desiredIdleSec atomic.Int64
}

// New returns an empty registry. A worker whose last heartbeat is older than
// staleAfter is treated as gone for placement.
func New(staleAfter time.Duration) *Registry {
	r := &Registry{workers: map[string]*workerState{}, staleAfter: staleAfter}
	r.desiredMaxWarm.Store(-1)
	r.desiredMinWarm.Store(-1)
	r.desiredIdleSec.Store(-1)
	return r
}

// SetMaxWarm sets the fleet-wide per-worker warm cap: applied to every worker
// on the next heartbeat poll and re-applied whenever a worker's reported cap
// drifts (a reboot restores its boot flag). Implements the dashboard's warm
// setting for a fleet.
func (r *Registry) SetMaxWarm(n int) {
	r.desiredMaxWarm.Store(int64(max(n, 0)))
}

// SetMinWarm sets the fleet-wide warm floor, reconciled onto workers like
// SetMaxWarm.
func (r *Registry) SetMinWarm(n int) {
	r.desiredMinWarm.Store(int64(max(n, 0)))
}

// SetIdleOverride sets the fleet-wide idle-timeout override (0 = restore
// per-manifest values), reconciled onto workers like SetMaxWarm.
func (r *Registry) SetIdleOverride(d time.Duration) {
	r.desiredIdleSec.Store(int64(max(d, 0) / time.Second))
}

// Add registers (or replaces) a worker by id.
func (r *Registry) Add(id string, be Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workers[id] = &workerState{id: id, be: be}
}

// Remove deregisters a worker.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.workers, id)
}

func (r *Registry) list() []*workerState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*workerState, 0, len(r.workers))
	for _, w := range r.workers {
		out = append(out, w)
	}
	return out
}

// recordHeartbeat updates a worker's cached capacity. Exposed for the poll loop
// and for tests.
func (r *Registry) recordHeartbeat(id string, hb workerapi.Heartbeat, at time.Time, reachable bool) {
	r.mu.RLock()
	w := r.workers[id]
	r.mu.RUnlock()
	if w == nil {
		return
	}
	w.mu.Lock()
	if reachable {
		w.hb = hb
	}
	w.lastBeat = at
	w.reachable = reachable
	w.mu.Unlock()
}

// StartHeartbeats polls every worker's Heartbeat immediately and then on
// interval, until ctx is cancelled. Returns after launching the loop.
func (r *Registry) StartHeartbeats(ctx context.Context, interval time.Duration) {
	poll := func() {
		var wg sync.WaitGroup
		for _, w := range r.list() {
			wg.Add(1)
			go func(w *workerState) {
				defer wg.Done()
				hctx, cancel := context.WithTimeout(ctx, interval)
				defer cancel()
				hb, err := w.be.Heartbeat(hctx)
				r.recordHeartbeat(w.id, hb, time.Now(), err == nil)
				// Reconcile the dashboard-configured settings: a worker
				// reporting different values (fresh boot, missed push) gets
				// them re-applied here, so the settings survive the whole
				// fleet's churn without any worker-side persistence.
				if err == nil {
					var cfg workerapi.WorkerConfig
					if want := r.desiredMaxWarm.Load(); want >= 0 && hb.MaxWarm != int(want) {
						n := int(want)
						cfg.MaxWarm = &n
					}
					if want := r.desiredMinWarm.Load(); want >= 0 && hb.MinWarm != int(want) {
						n := int(want)
						cfg.MinWarm = &n
					}
					if want := r.desiredIdleSec.Load(); want >= 0 && hb.IdleTimeoutSeconds != int(want) {
						n := int(want)
						cfg.IdleTimeoutSeconds = &n
					}
					if cfg.MaxWarm != nil || cfg.MinWarm != nil || cfg.IdleTimeoutSeconds != nil {
						w.be.Configure(hctx, cfg) //nolint:errcheck // next poll retries
					}
				}
			}(w)
		}
		wg.Wait()
	}
	go func() {
		poll()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				poll()
			}
		}
	}()
}

// EnsureRunning places k on a worker and starts (or reuses) the process there,
// returning the host:port the proxy dials. Implements proxy.Backends.
func (r *Registry) EnsureRunning(ctx context.Context, k supervise.Key, repoName string) (string, error) {
	w := r.place(k)
	if w == nil {
		return "", ErrNoWorker
	}
	return w.be.EnsureRunning(ctx, k, repoName)
}

// Capacity totals committed warm slots (running) and the *bounded* fleet
// capacity (the sum of positive max_warm caps) across fresh workers. A worker
// with max_warm 0 (unlimited) contributes no finite capacity figure — see
// LoadRatio for how unlimited headroom is scored — so it never inflates the
// denominator into implying the fleet is full.
func (r *Registry) Capacity() (running, capacity int) {
	now := time.Now()
	for _, w := range r.list() {
		hb, last, reachable := w.snapshot()
		if !r.fresh(last, reachable, now) {
			continue
		}
		running += hb.Running
		if hb.MaxWarm > 0 {
			capacity += hb.MaxWarm
		}
	}
	return running, capacity
}

// FleetStat is the fleet-wide runtime summary the dashboard's statistics view
// renders: how many workers are fresh, their committed and bounded capacity,
// and the cumulative warm/cold EnsureRunning counts summed across them.
type FleetStat struct {
	Workers    int   `json:"workers"`
	Running    int   `json:"running"`
	Capacity   int   `json:"capacity"` // 0 = unbounded (some fresh worker is unlimited)
	WarmHits   int64 `json:"warm_hits"`
	ColdStarts int64 `json:"cold_starts"`
}

// Stat summarizes the fresh workers for the dashboard. Capacity is 0 when any
// fresh worker is unlimited (max_warm 0), matching Capacity's convention that
// an unlimited worker contributes no finite bound.
func (r *Registry) Stat() FleetStat {
	now := time.Now()
	var st FleetStat
	var unbounded bool
	for _, w := range r.list() {
		hb, last, reachable := w.snapshot()
		if !r.fresh(last, reachable, now) {
			continue
		}
		st.Workers++
		st.Running += hb.Running
		if hb.MaxWarm > 0 {
			st.Capacity += hb.MaxWarm
		} else {
			unbounded = true
		}
		st.WarmHits += hb.WarmHits
		st.ColdStarts += hb.ColdStarts
	}
	if unbounded {
		st.Capacity = 0
	}
	return st
}

// LoadRatio is committed warm slots ÷ fleet capacity in [0,1] — the target a
// scale-out policy tracks. Any fresh worker with unlimited capacity (max_warm
// 0) means the fleet has spare room, so the ratio is 0 (never a scale-out
// trigger) rather than running/running = 1, which would read as "scale out
// forever". With no fresh worker, or all bounded workers at zero capacity, it
// is 1.
func (r *Registry) LoadRatio() float64 {
	now := time.Now()
	var running, capacity int
	var anyFresh, unbounded bool
	for _, w := range r.list() {
		hb, last, reachable := w.snapshot()
		if !r.fresh(last, reachable, now) {
			continue
		}
		anyFresh = true
		running += hb.Running
		if hb.MaxWarm > 0 {
			capacity += hb.MaxWarm
		} else {
			unbounded = true
		}
	}
	if unbounded {
		return 0
	}
	if !anyFresh || capacity <= 0 {
		return 1
	}
	return float64(running) / float64(capacity)
}

func (r *Registry) fresh(last time.Time, reachable bool, now time.Time) bool {
	return reachable && !last.IsZero() && now.Sub(last) < r.staleAfter
}

// place chooses a worker for k: the highest-weight rendezvous worker among
// those with free capacity; failing that (all full), the least-loaded fresh,
// non-draining worker; nil if none qualifies.
func (r *Registry) place(k supervise.Key) *workerState {
	key := placementKey(k)
	now := time.Now()

	var eligible, fallback []*workerState
	for _, w := range r.list() {
		hb, last, reachable := w.snapshot()
		if !r.fresh(last, reachable, now) || hb.Draining {
			continue
		}
		fallback = append(fallback, w)
		if hb.MaxWarm <= 0 || hb.Running < hb.MaxWarm {
			eligible = append(eligible, w)
		}
	}

	if len(eligible) > 0 {
		return topWeight(eligible, key)
	}
	// All fresh workers are full: place on the least-loaded so the preview still
	// serves (a per-worker cap is a soft target, not a hard refusal), ties broken
	// by rendezvous weight for stability.
	return leastLoaded(fallback, key)
}

// placementKey is what the hash is taken over. A process-mode frontend hashes
// on its backend's hash so it co-locates with the backend it shares a deploy
// network with; everything else hashes on its own artifact hash. Namespaced by
// repo so two repos' identical hashes (unlikely, but content-addresses can
// collide across trivial artifacts) don't force co-placement.
func placementKey(k supervise.Key) string {
	h := k.Hash
	if k.Side == supervise.SideFrontend && k.Peer != "" {
		h = k.Peer
	}
	return strconv.FormatInt(k.RepoID, 10) + "/" + h
}

// topWeight returns the worker with the highest rendezvous weight for key.
func topWeight(workers []*workerState, key string) *workerState {
	var best *workerState
	var bestScore uint64
	for _, w := range workers {
		s := hrw(w.id, key)
		if best == nil || s > bestScore {
			best, bestScore = w, s
		}
	}
	return best
}

// leastLoaded returns the worker with the smallest running/max_warm ratio;
// rendezvous weight breaks ties so placement stays stable.
func leastLoaded(workers []*workerState, key string) *workerState {
	var best *workerState
	var bestLoad float64
	var bestScore uint64
	for _, w := range workers {
		hb, _, _ := w.snapshot()
		load := 0.0
		if hb.MaxWarm > 0 {
			load = float64(hb.Running) / float64(hb.MaxWarm)
		}
		s := hrw(w.id, key)
		if best == nil || load < bestLoad || (load == bestLoad && s > bestScore) {
			best, bestLoad, bestScore = w, load, s
		}
	}
	return best
}

// hrw is the rendezvous weight of (workerID, key): a deterministic hash, so a
// key's ranking of workers is stable and independent of registration order.
func hrw(workerID, key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(workerID))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return h.Sum64()
}
