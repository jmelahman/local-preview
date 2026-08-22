package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/supervise"
	"github.com/jmelahman/local-preview/internal/workerapi"
)

type fakeBackend struct {
	id         string
	ensured    []supervise.Key
	report     []supervise.ProcReport
	logs       map[string]supervise.RunLog // keyed side+"-"+hash
	stopped    []supervise.Key
	configured []int
}

func (f *fakeBackend) EnsureRunning(_ context.Context, k supervise.Key, _ string) (string, error) {
	f.ensured = append(f.ensured, k)
	return f.id + ":8000", nil
}
func (f *fakeBackend) Heartbeat(context.Context) (workerapi.Heartbeat, error) {
	return workerapi.Heartbeat{}, nil
}
func (f *fakeBackend) Report(context.Context) ([]supervise.ProcReport, error) {
	return f.report, nil
}
func (f *fakeBackend) RunLog(_ context.Context, _, side, hash string, _ int, _ int64) (supervise.RunLog, error) {
	return f.logs[side+"-"+hash], nil
}
func (f *fakeBackend) Stop(_ context.Context, k supervise.Key, _ string) error {
	f.stopped = append(f.stopped, k)
	return nil
}
func (f *fakeBackend) Configure(_ context.Context, cfg workerapi.WorkerConfig) error {
	if cfg.MaxWarm != nil {
		f.configured = append(f.configured, *cfg.MaxWarm)
	}
	return nil
}

// regFresh registers workers with the given heartbeats, all beating "now".
func regFresh(t *testing.T, hbs map[string]workerapi.Heartbeat) (*Registry, map[string]*fakeBackend) {
	t.Helper()
	r := New(time.Minute)
	bes := map[string]*fakeBackend{}
	for id, hb := range hbs {
		be := &fakeBackend{id: id}
		bes[id] = be
		r.Add(id, "", be)
		r.recordHeartbeat(id, hb, time.Now(), true)
	}
	return r, bes
}

func TestCoPlacesFrontendWithBackend(t *testing.T) {
	r, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"w1": {Running: 0, MaxWarm: 10},
		"w2": {Running: 0, MaxWarm: 10},
		"w3": {Running: 0, MaxWarm: 10},
	})
	be := r.place(supervise.BackendKey(42, "beHASH"))
	fe := r.place(supervise.FrontendKey(42, "feHASH", "beHASH"))
	if be == nil || fe == nil {
		t.Fatal("expected placements")
	}
	if be.id != fe.id {
		t.Fatalf("frontend placed on %s but its backend on %s — must co-locate", fe.id, be.id)
	}
	// And placement is deterministic.
	if again := r.place(supervise.BackendKey(42, "beHASH")); again.id != be.id {
		t.Fatalf("non-deterministic placement: %s then %s", be.id, again.id)
	}
}

func TestDrainingExcluded(t *testing.T) {
	r, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"only": {Running: 0, MaxWarm: 10, Draining: true},
	})
	if w := r.place(supervise.BackendKey(1, "h")); w != nil {
		t.Fatalf("placed on a draining worker %s", w.id)
	}
	if _, err := r.EnsureRunning(context.Background(), supervise.BackendKey(1, "h"), "demo"); !errors.Is(err, ErrNoWorker) {
		t.Fatalf("err = %v, want ErrNoWorker", err)
	}
}

func TestFullFallsBackToLeastLoaded(t *testing.T) {
	// Both workers are at capacity; placement must still pick one (soft cap).
	r, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"busy":   {Running: 10, MaxWarm: 10},
		"busier": {Running: 20, MaxWarm: 10},
	})
	w := r.place(supervise.BackendKey(1, "h"))
	if w == nil {
		t.Fatal("expected a fallback placement when all are full")
	}
	if w.id != "busy" {
		t.Fatalf("fallback chose %s, want the least-loaded 'busy'", w.id)
	}
}

func TestPrefersWorkerWithFreeCapacity(t *testing.T) {
	// "free" has a slot; "full" does not — regardless of hash, the eligible one wins.
	r, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"free": {Running: 1, MaxWarm: 10},
		"full": {Running: 10, MaxWarm: 10},
	})
	for i := 0; i < 5; i++ {
		if w := r.place(supervise.BackendKey(int64(i), "h")); w.id != "free" {
			t.Fatalf("placed on %s, want the only worker with capacity", w.id)
		}
	}
}

func TestNoWorkersOrAllStale(t *testing.T) {
	r := New(time.Minute)
	if _, err := r.EnsureRunning(context.Background(), supervise.BackendKey(1, "h"), "demo"); !errors.Is(err, ErrNoWorker) {
		t.Fatalf("empty registry err = %v, want ErrNoWorker", err)
	}
	// A stale heartbeat is as good as gone.
	r.Add("w", "", &fakeBackend{id: "w"})
	r.recordHeartbeat("w", workerapi.Heartbeat{MaxWarm: 10}, time.Now().Add(-time.Hour), true)
	if w := r.place(supervise.BackendKey(1, "h")); w != nil {
		t.Fatalf("placed on stale worker %s", w.id)
	}
}

// Unserved demand counts previews waiting for capacity, not raw request
// retries: N retries of one preview must read as demand 1 or the scale-out
// policy launches nodes for phantom load, while a frontend co-placed with its
// backend shares the backend's placement key (one preview, one demand).
func TestDrainDemandDedupes(t *testing.T) {
	r := New(time.Minute)
	ctx := context.Background()
	for range 5 {
		r.EnsureRunning(ctx, supervise.BackendKey(1, "beA"), "demo") //nolint:errcheck // ErrNoWorker is the point
	}
	r.EnsureRunning(ctx, supervise.FrontendKey(1, "feA", "beA"), "demo") //nolint:errcheck // co-placed: same key as beA
	r.EnsureRunning(ctx, supervise.BackendKey(1, "beB"), "demo")         //nolint:errcheck // a second waiting preview
	if got := r.DrainDemand(); got != 2 {
		t.Fatalf("DrainDemand = %d, want 2 (distinct previews, retries and co-placed sides deduped)", got)
	}
	// Drain resets: quiet interval reads 0, and a fresh miss counts anew.
	if got := r.DrainDemand(); got != 0 {
		t.Fatalf("DrainDemand after drain = %d, want 0", got)
	}
	r.EnsureRunning(ctx, supervise.BackendKey(1, "beA"), "demo") //nolint:errcheck // still unserved next interval
	if got := r.DrainDemand(); got != 1 {
		t.Fatalf("DrainDemand re-registered = %d, want 1", got)
	}
}

func TestCapacityAndLoad(t *testing.T) {
	r, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"a": {Running: 3, MaxWarm: 10},
		"b": {Running: 5, MaxWarm: 10},
	})
	running, capacity := r.Capacity()
	if running != 8 || capacity != 20 {
		t.Fatalf("capacity = %d/%d, want 8/20", running, capacity)
	}
	if got := r.LoadRatio(); got != 0.4 {
		t.Fatalf("load ratio = %v, want 0.4", got)
	}
}

func TestStat(t *testing.T) {
	r, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"a": {Running: 3, MaxWarm: 10, WarmHits: 40, ColdStarts: 5},
		"b": {Running: 5, MaxWarm: 10, WarmHits: 50, ColdStarts: 5},
	})
	st := r.Stat()
	if st.Workers != 2 || st.Running != 8 || st.Capacity != 20 {
		t.Fatalf("stat = %+v, want workers=2 running=8 capacity=20", st)
	}
	if st.WarmHits != 90 || st.ColdStarts != 10 {
		t.Fatalf("stat hits = %d/%d, want 90/10", st.WarmHits, st.ColdStarts)
	}

	// An unbounded worker zeroes the finite capacity figure (matches Capacity).
	unb, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"full":      {Running: 10, MaxWarm: 10},
		"unbounded": {Running: 3, MaxWarm: 0},
	})
	if got := unb.Stat(); got.Capacity != 0 || got.Workers != 2 {
		t.Fatalf("unbounded stat = %+v, want capacity=0 workers=2", got)
	}
}

func TestLoadRatioUnboundedIsHeadroom(t *testing.T) {
	// An unlimited worker (max_warm 0) means the fleet has spare room, so the
	// scale-out signal must read 0, not running/running = 1 ("scale forever").
	r, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"unbounded": {Running: 100, MaxWarm: 0},
	})
	if got := r.LoadRatio(); got != 0 {
		t.Fatalf("all-unbounded load = %v, want 0", got)
	}
	// Even mixed with a full bounded worker, the unbounded one is headroom.
	mixed, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"full":      {Running: 10, MaxWarm: 10},
		"unbounded": {Running: 3, MaxWarm: 0},
	})
	if got := mixed.LoadRatio(); got != 0 {
		t.Fatalf("mixed-with-unbounded load = %v, want 0", got)
	}
	// No workers at all: fully loaded (need capacity).
	if got := New(time.Minute).LoadRatio(); got != 1 {
		t.Fatalf("empty fleet load = %v, want 1", got)
	}
}

func TestEnsureRoutesToPlacedWorker(t *testing.T) {
	r, bes := regFresh(t, map[string]workerapi.Heartbeat{
		"w1": {Running: 0, MaxWarm: 10},
		"w2": {Running: 0, MaxWarm: 10},
	})
	k := supervise.BackendKey(9, "hh")
	placed := r.place(k)
	addr, err := r.EnsureRunning(context.Background(), k, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if addr != placed.id+":8000" {
		t.Fatalf("routed to %q, expected worker %s", addr, placed.id)
	}
	if len(bes[placed.id].ensured) != 1 {
		t.Fatalf("placed worker %s was not asked to ensure", placed.id)
	}
}

// TestStatSurvivesWorkerRestart: a worker's since-boot counters running
// backwards means it restarted (routine on spot); the registry banks the
// previous boot's totals so the fleet-wide sums stay monotonic.
func TestStatSurvivesWorkerRestart(t *testing.T) {
	r, _ := regFresh(t, map[string]workerapi.Heartbeat{
		"w": {MaxWarm: 10, WarmHits: 40, ColdStarts: 5},
	})
	// The worker reboots and reports fresh, smaller counters.
	r.recordHeartbeat("w", workerapi.Heartbeat{MaxWarm: 10, WarmHits: 2, ColdStarts: 1}, time.Now(), true)
	st := r.Stat()
	if st.WarmHits != 42 || st.ColdStarts != 6 {
		t.Fatalf("hits after restart = %d/%d, want 42/6 (banked 40/5 + fresh 2/1)", st.WarmHits, st.ColdStarts)
	}
}
