package workerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/manifest"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

// TestMain doubles as the supervised child (same trick as the supervise
// package's tests): re-executed with --helper-server it serves a health
// endpoint, so the worker-over-empty-DB test starts a real process.
func TestMain(m *testing.M) {
	if i := slices.Index(os.Args, "--helper-server"); i >= 0 && i+1 < len(os.Args) {
		http.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		http.ListenAndServe("127.0.0.1:"+os.Args[i+1], nil) //nolint:errcheck
		return
	}
	os.Exit(m.Run())
}

// fakeSup records calls and returns programmed results.
type fakeSup struct {
	mu         sync.Mutex
	ensureKey  supervise.Key
	ensureRepo string
	ensurePort int
	ensureErr  error
	offered    map[supervise.Key]supervise.WireSpec
	stopped    []supervise.Key
	status     string
	failDetail string
	report     []supervise.ProcReport
	runLog     supervise.RunLog
	running    int
	maxWarm    int
}

func (f *fakeSup) OfferWireSpec(k supervise.Key, s supervise.WireSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.offered == nil {
		f.offered = map[supervise.Key]supervise.WireSpec{}
	}
	f.offered[k] = s
}

func (f *fakeSup) EnsureRunning(_ context.Context, k supervise.Key, repo string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureKey = k
	f.ensureRepo = repo
	return f.ensurePort, f.ensureErr
}
func (f *fakeSup) Stop(k supervise.Key, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, k)
}
func (f *fakeSup) Status(k supervise.Key) string { return f.status }
func (f *fakeSup) LastFailure(k supervise.Key) (supervise.Failure, bool) {
	return supervise.Failure{Detail: f.failDetail}, f.failDetail != ""
}
func (f *fakeSup) Report(context.Context) []supervise.ProcReport { return f.report }
func (f *fakeSup) RunLog(repo, side, hash string, attempt int, offset int64) (supervise.RunLog, error) {
	return f.runLog, nil
}
func (f *fakeSup) Running() int { return f.running }
func (f *fakeSup) MaxWarm() int { return f.maxWarm }

const secret = "s3cr3t"

func testServer(t *testing.T, sup Supervisor) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(NewServer(sup, secret).Handler())
	c := NewClient(srv.URL, "10.0.0.7", secret, srv.Client())
	return c, srv.Close
}

func TestEnsureRoundTrip(t *testing.T) {
	sup := &fakeSup{ensurePort: 42123}
	c, done := testServer(t, sup)
	defer done()

	k := supervise.FrontendKey(7, "feHASH", "beHASH")
	addr, err := c.EnsureRunning(context.Background(), k, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.0.7:42123" {
		t.Fatalf("addr = %q, want 10.0.0.7:42123", addr)
	}
	if sup.ensureKey != k || sup.ensureRepo != "demo" {
		t.Fatalf("worker saw key=%+v repo=%q", sup.ensureKey, sup.ensureRepo)
	}
}

func TestEnsureErrorPropagates(t *testing.T) {
	sup := &fakeSup{ensureErr: errors.New("artifact files missing (evicted?)")}
	c, done := testServer(t, sup)
	defer done()
	_, err := c.EnsureRunning(context.Background(), supervise.BackendKey(1, "h"), "demo")
	if err == nil || err.Error() != "artifact files missing (evicted?)" {
		t.Fatalf("err = %v, want the worker's detail", err)
	}
}

func TestStopAndStatus(t *testing.T) {
	sup := &fakeSup{status: supervise.StatusRunning}
	c, done := testServer(t, sup)
	defer done()

	k := supervise.BackendKey(3, "h3")
	if err := c.Stop(context.Background(), k, "manual"); err != nil {
		t.Fatal(err)
	}
	if len(sup.stopped) != 1 || sup.stopped[0] != k {
		t.Fatalf("stopped = %v, want [%v]", sup.stopped, k)
	}
	st, _, err := c.Status(context.Background(), k)
	if err != nil || st != supervise.StatusRunning {
		t.Fatalf("status = %q, %v", st, err)
	}
}

// TestStatusCarriesFailureDetail: a crashed status travels with its cause.
func TestStatusCarriesFailureDetail(t *testing.T) {
	sup := &fakeSup{status: supervise.StatusCrashed, failDetail: "exit status 3"}
	c, done := testServer(t, sup)
	defer done()
	st, detail, err := c.Status(context.Background(), supervise.BackendKey(1, "h"))
	if err != nil || st != supervise.StatusCrashed || detail != "exit status 3" {
		t.Fatalf("status = %q detail = %q, %v", st, detail, err)
	}
}

// TestReportAndRunLogRoundTrip: the bulk report and run-log slices cross the
// wire intact — keys, stats, and chunk fields alike.
func TestReportAndRunLogRoundTrip(t *testing.T) {
	cpu := 12.5
	k := supervise.FrontendKey(7, "feHASH", "beHASH")
	sup := &fakeSup{
		report: []supervise.ProcReport{{
			Key: k, Repo: "demo", Status: supervise.StatusRunning,
			Stats: &supervise.ProcessStats{Runtime: "container", CPUPercent: &cpu, MemoryBytes: 42, MemoryLimitBytes: 100},
		}, {
			Key: supervise.BackendKey(7, "beHASH"), Status: supervise.StatusCrashed, Error: "boom",
		}},
		runLog: supervise.RunLog{Attempt: 3, Offset: 17, Content: "hello", Truncated: true},
	}
	c, done := testServer(t, sup)
	defer done()

	procs, err := c.Report(context.Background())
	if err != nil || len(procs) != 2 {
		t.Fatalf("report = %+v, %v", procs, err)
	}
	if procs[0].Key != k || procs[0].Repo != "demo" || procs[0].Stats == nil || *procs[0].Stats.CPUPercent != cpu {
		t.Fatalf("proc[0] = %+v", procs[0])
	}
	if procs[1].Status != supervise.StatusCrashed || procs[1].Error != "boom" {
		t.Fatalf("proc[1] = %+v", procs[1])
	}

	chunk, err := c.RunLog(context.Background(), "demo", "fe", "feHASH", 0, 0)
	if err != nil || chunk != sup.runLog {
		t.Fatalf("runlog = %+v, %v", chunk, err)
	}
}

func TestHeartbeat(t *testing.T) {
	sup := &fakeSup{running: 5, maxWarm: 12}
	c, done := testServer(t, sup)
	defer done()
	hb, err := c.Heartbeat(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if hb.Running != 5 || hb.MaxWarm != 12 || hb.Draining {
		t.Fatalf("heartbeat = %+v", hb)
	}
}

// TestEnsureWireSpecOnEmptyDB is the regression test for the worker-tier gap
// the fakes cannot see: a real supervise.Manager over a fresh, empty DB — a
// --role=worker node — must start a process from the wire-supplied spec
// alone. Before specs traveled on the wire, this failed "backend artifact not
// provisioned" and every remote ensure was a 502.
func TestEnsureWireSpecOnEmptyDB(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	root := t.TempDir()
	files := store.New(
		filepath.Join(root, "artifacts"),
		filepath.Join(root, "state"),
		filepath.Join(root, "tmp"),
	)
	m := supervise.New(database, files, filepath.Join(root, "logs"))
	t.Cleanup(m.StopAll)

	// The artifact *files* are resident (in production: hydrated from the S3
	// tier); only the DB rows are absent.
	const beHash = "behash4worker001"
	scratch, _, err := files.NewScratchDir("be")
	if err != nil {
		t.Fatal(err)
	}
	if err := files.PublishBackend("demo", beHash, scratch, false); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(manifest.Backend{
		Run:          []string{exe, "--helper-server", "{port}"},
		HealthPath:   "/api/health",
		StartTimeout: manifest.Duration(10 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(NewServer(m, secret).Handler())
	defer srv.Close()
	c := NewClient(srv.URL, "127.0.0.1", secret, srv.Client())
	c.SpecResolver = func(k supervise.Key) (supervise.WireSpec, error) {
		return supervise.WireSpec{RunConfig: string(raw)}, nil
	}

	addr, err := c.EnsureRunning(context.Background(), supervise.BackendKey(1, beHash), "demo")
	if err != nil {
		t.Fatalf("ensure over empty DB with wire spec: %v", err)
	}
	res, err := http.Get(fmt.Sprintf("http://%s/api/health", addr))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health = %d, want 200", res.StatusCode)
	}
	// The worker recomputed the state dir against its own store root.
	if _, err := os.Stat(files.StateDirPath("demo", beHash)); err != nil {
		t.Fatalf("worker-local state dir: %v", err)
	}
}

// TestEnsureCarriesSpecs asserts the wire shape: the resolver's spec (and the
// peer backend's, for a process-mode frontend) reach the worker's supervisor.
func TestEnsureCarriesSpecs(t *testing.T) {
	sup := &fakeSup{ensurePort: 1}
	c, done := testServer(t, sup)
	defer done()
	c.SpecResolver = func(k supervise.Key) (supervise.WireSpec, error) {
		return supervise.WireSpec{RunConfig: `{"side":"` + string(k.Side) + `"}`, InitDone: k.Side == supervise.SideBackend}, nil
	}

	k := supervise.FrontendKey(7, "feHASH", "beHASH")
	if _, err := c.EnsureRunning(context.Background(), k, "demo"); err != nil {
		t.Fatal(err)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	fe, ok := sup.offered[k]
	if !ok || fe.RunConfig != `{"side":"fe"}` || fe.InitDone {
		t.Fatalf("frontend spec = %+v, %v", fe, ok)
	}
	be, ok := sup.offered[supervise.BackendKey(7, "beHASH")]
	if !ok || be.RunConfig != `{"side":"be"}` || !be.InitDone {
		t.Fatalf("peer backend spec = %+v, %v", be, ok)
	}
}

func TestAuthRequired(t *testing.T) {
	sup := &fakeSup{}
	srv := httptest.NewServer(NewServer(sup, secret).Handler())
	defer srv.Close()

	// No secret → 401.
	bad := NewClient(srv.URL, "h", "wrong-secret", srv.Client())
	if _, err := bad.Heartbeat(context.Background()); err == nil {
		t.Fatal("expected auth failure with the wrong secret")
	}

	// Raw request with no header at all → 401.
	res, err := srv.Client().Get(srv.URL + pathHeartbeat)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", res.StatusCode)
	}
}
