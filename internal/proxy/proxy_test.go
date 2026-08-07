package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

// fakeBackends satisfies Backends without real processes.
type fakeBackends struct {
	port    int
	err     error
	slow    bool // simulate a cold start that outlives the request's patience
	lastKey supervise.Key
}

func (f *fakeBackends) EnsureRunning(ctx context.Context, k supervise.Key, repoName string) (int, error) {
	f.lastKey = k
	if f.slow {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if f.err != nil {
		return 0, f.err
	}
	return f.port, nil
}

type testEnv struct {
	router *Router
	db     *db.Store
	files  *store.Store
	repoID int64
	fake   *fakeBackends
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
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
	repo, err := database.CreateRepo("demo", "/src", "/bare", db.RepoReady)
	if err != nil {
		t.Fatal(err)
	}
	dashboard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "dashboard")
	})
	fake := &fakeBackends{}
	router := New(database, files, fake, "preview.localhost", dashboard)
	return &testEnv{router: router, db: database, files: files, repoID: repo.ID, fake: fake}
}

// readyDeploy publishes a frontend artifact and inserts a ready deploy row.
func (e *testEnv) readyDeploy(t *testing.T, sha string) db.Deploy {
	t.Helper()
	scratch, _, err := e.files.NewScratchDir("fe")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "index.html"), []byte("<html>preview home</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scratch, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "assets", "app.js"), []byte("js-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	feHash := "fe" + sha[:8]
	if err := e.files.PublishFrontend("demo", feHash, scratch, false); err != nil {
		t.Fatal(err)
	}
	d, err := e.db.CreateDeploy(e.repoID, sha, db.DeployMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetDeployHashes(d.ID, feHash, "be"+sha[:8], "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetDeployReady(d.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.db.GetDeployBySHA(e.repoID, sha)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func doReq(t *testing.T, router *Router, host, path string, browser bool) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://"+host+path, nil)
	req.Host = host
	if browser {
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body), rec.Result().Header
}

const shaOne = "1a2b3c4d5e6f7a8b9c0d1a2b3c4d5e6f7a8b9c0d"

func TestNonPreviewHostsGetDashboard(t *testing.T) {
	e := newTestEnv(t)
	for _, host := range []string{
		"localhost:8080", "127.0.0.1:8080", "preview.localhost:8080",
		"preview.localhost", "example.com", "too.many.labels.demo.preview.localhost",
	} {
		code, body, _ := doReq(t, e.router, host, "/", true)
		if code != 200 || body != "dashboard" {
			t.Errorf("host %q: %d %q, want dashboard", host, code, body)
		}
	}
}

func TestPreviewStaticAndSPAFallback(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	host := d.ShortSHA + ".demo.preview.localhost:8080"

	code, body, _ := doReq(t, e.router, host, "/", true)
	if code != 200 || !strings.Contains(body, "preview home") {
		t.Fatalf("index: %d %q", code, body)
	}
	code, body, hdr := doReq(t, e.router, host, "/assets/app.js", false)
	if code != 200 || body != "js-content" {
		t.Fatalf("asset: %d %q", code, body)
	}
	// A --rebuild of the same sha may change files under the same URL, so
	// preview responses must revalidate rather than cache heuristically —
	// and never claim immutability.
	if got := hdr.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("asset Cache-Control = %q, want no-cache", got)
	}
	// SPA fallback for client-side routes.
	code, body, _ = doReq(t, e.router, host, "/some/client/route", true)
	if code != 200 || !strings.Contains(body, "preview home") {
		t.Fatalf("spa fallback: %d %q", code, body)
	}
}

func TestPreviewAPIProxied(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)

	var gotHost, gotForwarded string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotForwarded = r.Header.Get("X-Forwarded-Host")
		fmt.Fprintf(w, "upstream:%s", r.URL.Path)
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())
	e.fake.port = port

	host := d.ShortSHA + ".demo.preview.localhost:8080"
	code, body, _ := doReq(t, e.router, host, "/api/things", false)
	if code != 200 || body != "upstream:/api/things" {
		t.Fatalf("api proxy: %d %q", code, body)
	}
	// The backend must see itself as the host (Host-routing apps would
	// misroute otherwise); the preview host travels in X-Forwarded-Host.
	if gotHost != u.Host {
		t.Fatalf("upstream Host = %q, want %q", gotHost, u.Host)
	}
	if gotForwarded != host {
		t.Fatalf("X-Forwarded-Host = %q, want %q", gotForwarded, host)
	}
}

// TestProcessFrontendRouting covers the container-era routing shape: a
// process-mode frontend receives page traffic, /api is prefix-stripped when
// the backend asks, and extra routes reach the backend unstripped.
func TestProcessFrontendRouting(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)

	// Mark the frontend as a process and give the backend routing config.
	if err := e.db.CreateFrontendArtifact(db.FrontendArtifact{
		RepoID: e.repoID, FeHash: d.FeHash, RunConfig: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.CreateBackendArtifact(db.BackendArtifact{
		RepoID: e.repoID, BeHash: d.BeHash, StateDir: "/tmp/none",
		RunConfig: `{"strip_api_prefix":true,"extra_routes":["/openapi.json","/auth/saml"]}`,
	}); err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "upstream:%s", r.URL.Path)
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())
	e.fake.port = port

	host := d.ShortSHA + ".demo.preview.localhost:8080"
	cases := []struct {
		path     string
		wantBody string
		wantSide supervise.Side
	}{
		{"/", "upstream:/", supervise.SideFrontend},
		{"/chat/session", "upstream:/chat/session", supervise.SideFrontend},
		{"/api/things", "upstream:/things", supervise.SideBackend}, // stripped
		{"/openapi.json", "upstream:/openapi.json", supervise.SideBackend},
		{"/auth/saml/callback", "upstream:/auth/saml/callback", supervise.SideBackend},
	}
	for _, tc := range cases {
		code, body, _ := doReq(t, e.router, host, tc.path, false)
		if code != 200 || body != tc.wantBody {
			t.Errorf("%s: %d %q, want %q", tc.path, code, body, tc.wantBody)
		}
		if e.fake.lastKey.Side != tc.wantSide {
			t.Errorf("%s routed to side %q, want %q", tc.path, e.fake.lastKey.Side, tc.wantSide)
		}
	}
	// The frontend key pairs the fe artifact with this deploy's backend.
	if e.fake.lastKey = (supervise.Key{}); true {
		doReq(t, e.router, host, "/page", false)
	}
	if e.fake.lastKey.Hash != d.FeHash || e.fake.lastKey.Peer != d.BeHash {
		t.Fatalf("frontend key = %+v", e.fake.lastKey)
	}
}

func TestColdStartAndCrash(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	host := d.ShortSHA + ".demo.preview.localhost:8080"

	// Still starting: JSON callers get 503 + Retry-After.
	e.fake.slow = true
	code, body, hdr := doReq(t, e.router, host, "/api/x", false)
	if code != 503 || hdr.Get("Retry-After") == "" || !strings.Contains(body, "Starting") {
		t.Fatalf("cold start: %d %q %v", code, body, hdr)
	}

	// Crash: 502 with the failure detail.
	e.fake.slow = false
	e.fake.err = errors.New("backend exited during startup")
	code, body, _ = doReq(t, e.router, host, "/api/x", false)
	if code != 502 || !strings.Contains(body, "exited") {
		t.Fatalf("crash: %d %q", code, body)
	}
}

func TestNonReadyStatuses(t *testing.T) {
	e := newTestEnv(t)
	d, err := e.db.CreateDeploy(e.repoID, shaOne, db.DeployMeta{})
	if err != nil {
		t.Fatal(err)
	}
	host := d.ShortSHA + ".demo.preview.localhost:8080"

	code, body, _ := doReq(t, e.router, host, "/", true)
	if code != 503 || !strings.Contains(body, "refresh") {
		t.Fatalf("queued page: %d %q", code, body)
	}

	if err := e.db.SetDeployFailed(d.ID, "compile exploded"); err != nil {
		t.Fatal(err)
	}
	// Past the routing-cache TTL concerns: use a fresh router to avoid
	// waiting out the TTL in tests.
	e.router.cache = map[string]cacheEntry{}
	code, body, _ = doReq(t, e.router, host, "/", true)
	if code != 502 || !strings.Contains(body, "compile exploded") {
		t.Fatalf("failed page: %d %q", code, body)
	}
}

func TestAmbiguousAndUnknown(t *testing.T) {
	e := newTestEnv(t)
	// Two shas sharing a 7-char prefix; short_shas grew to 8.
	shaX := "abcdef0" + strings.Repeat("1", 33)
	shaY := "abcdef0" + strings.Repeat("2", 33)
	e.readyDeploy(t, shaX)
	e.readyDeploy(t, shaY)

	code, body, _ := doReq(t, e.router, "abcdef0.demo.preview.localhost", "/", true)
	if code != 404 || !strings.Contains(body, "Ambiguous") {
		t.Fatalf("ambiguous: %d %q", code, body)
	}
	// A full unique prefix works.
	code, _, _ = doReq(t, e.router, "abcdef01.demo.preview.localhost", "/", true)
	if code != 200 {
		t.Fatalf("unique prefix: %d", code)
	}

	code, body, _ = doReq(t, e.router, "1234567.nope.preview.localhost", "/", true)
	if code != 404 || !strings.Contains(body, "No repo") {
		t.Fatalf("unknown repo: %d %q", code, body)
	}
	code, body, _ = doReq(t, e.router, "fffffff.demo.preview.localhost", "/", true)
	if code != 404 || !strings.Contains(body, "No deploy") {
		t.Fatalf("unknown deploy: %d %q", code, body)
	}
	code, body, _ = doReq(t, e.router, "not-hex.demo.preview.localhost", "/", true)
	if code != 404 || !strings.Contains(body, "not a sha prefix") {
		t.Fatalf("non-hex label: %d %q", code, body)
	}
}

func TestParseHost(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		host        string
		label, repo string
		ok          bool
	}{
		{"abc1234.demo.preview.localhost:8080", "abc1234", "demo", true},
		{"ABC1234.DEMO.PREVIEW.LOCALHOST", "abc1234", "demo", true},
		{"abc1234.demo.preview.localhost.", "abc1234", "demo", true},
		{"preview.localhost", "", "", false},
		{"demo.preview.localhost", "", "", false},
		{"a.b.c.preview.localhost", "", "", false},
		{"localhost:8080", "", "", false},
		{"[::1]:8080", "", "", false},
	}
	for _, tc := range cases {
		label, repo, ok := e.router.parseHost(tc.host)
		if label != tc.label || repo != tc.repo || ok != tc.ok {
			t.Errorf("parseHost(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.host, label, repo, ok, tc.label, tc.repo, tc.ok)
		}
	}
}
