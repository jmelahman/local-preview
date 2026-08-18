package workerapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jmelahman/local-preview/internal/supervise"
)

// fakeSup records calls and returns programmed results.
type fakeSup struct {
	mu         sync.Mutex
	ensureKey  supervise.Key
	ensureRepo string
	ensurePort int
	ensureErr  error
	stopped    []supervise.Key
	status     string
	running    int
	maxWarm    int
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
func (f *fakeSup) Running() int                  { return f.running }
func (f *fakeSup) MaxWarm() int                  { return f.maxWarm }

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
	st, err := c.Status(context.Background(), k)
	if err != nil || st != supervise.StatusRunning {
		t.Fatalf("status = %q, %v", st, err)
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
