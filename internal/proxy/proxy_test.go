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
	"time"

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

func (f *fakeBackends) EnsureRunning(ctx context.Context, k supervise.Key, repoName string) (string, error) {
	f.lastKey = k
	if f.slow {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if f.err != nil {
		return "", f.err
	}
	return "127.0.0.1:" + strconv.Itoa(f.port), nil
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
		"preview.localhost", "example.com", "too.many.labels.preview.localhost",
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
	host := d.ShortSHA + "-demo.preview.localhost:8080"

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

	host := d.ShortSHA + "-demo.preview.localhost:8080"
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

	host := d.ShortSHA + "-demo.preview.localhost:8080"
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
	host := d.ShortSHA + "-demo.preview.localhost:8080"

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

// fakeRunLogs satisfies RunLogs, recording the cursor it was asked for.
type fakeRunLogs struct {
	side        string
	hash        string
	lastAttempt int
	lastOffset  int64
}

func (f *fakeRunLogs) RunLog(repo, side, hash string, attempt int, offset int64) (supervise.RunLog, error) {
	f.side, f.hash = side, hash
	f.lastAttempt, f.lastOffset = attempt, offset
	return supervise.RunLog{Attempt: 3, Offset: offset + 12, Content: "boot line\n"}, nil
}

func doPoll(t *testing.T, router *Router, host, path string, attempt int, offset int64) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://"+host+path, nil)
	req.Host = host
	req.Header.Set("X-Preview-Poll", "1")
	req.Header.Set("X-Preview-Log-Attempt", strconv.Itoa(attempt))
	req.Header.Set("X-Preview-Log-Offset", strconv.FormatInt(offset, 10))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body), rec.Result().Header
}

// TestInterimPoll covers the interim page's poll protocol: while the process
// starts, polls stream incremental run-log chunks; a start failure reports
// "failed" in place; a ready preview answers without the interim marker (the
// page's cue to reload).
func TestInterimPoll(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	host := d.ShortSHA + "-demo.preview.localhost:8080"
	logs := &fakeRunLogs{}
	e.router.SetRunLogs(logs)

	// Still starting: JSON with the interim marker and the log slice from the
	// echoed cursor.
	e.fake.slow = true
	code, body, hdr := doPoll(t, e.router, host, "/api/x", 3, 100)
	if code != 503 || hdr.Get("X-Preview-Interim") != "starting" {
		t.Fatalf("starting poll: %d %v", code, hdr)
	}
	if !strings.Contains(body, `"state":"starting"`) || !strings.Contains(body, "boot line") ||
		!strings.Contains(body, `"attempt":3`) || !strings.Contains(body, `"offset":112`) {
		t.Fatalf("starting poll body: %q", body)
	}
	if logs.side != "be" || logs.hash != d.BeHash || logs.lastAttempt != 3 || logs.lastOffset != 100 {
		t.Fatalf("run-log cursor: %+v", logs)
	}

	// Start failure: reported in place as "failed", still marked interim so
	// the page shows the error beside the captured logs instead of reloading.
	e.fake.slow = false
	e.fake.err = errors.New("backend exited during startup")
	code, body, hdr = doPoll(t, e.router, host, "/api/x", 3, 112)
	if code != 502 || hdr.Get("X-Preview-Interim") != "failed" ||
		!strings.Contains(body, `"state":"failed"`) || !strings.Contains(body, "exited") {
		t.Fatalf("failed poll: %d %v %q", code, hdr, body)
	}

	// Ready: the poll reaches the app, with no interim marker → the page reloads.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "app answer")
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	e.fake.err = nil
	e.fake.port, _ = strconv.Atoi(u.Port())
	code, body, hdr = doPoll(t, e.router, host, "/api/x", 3, 112)
	if code != 200 || hdr.Get("X-Preview-Interim") != "" || body != "app answer" {
		t.Fatalf("ready poll: %d %v %q", code, hdr, body)
	}
}

// The browser-facing "starting" page is the poller, not a meta refresh.
func TestColdStartBrowserGetsPollerPage(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	host := d.ShortSHA + "-demo.preview.localhost:8080"
	e.fake.slow = true

	code, body, hdr := doReq(t, e.router, host, "/api/x", true)
	if code != 503 || hdr.Get("X-Preview-Interim") != "starting" ||
		!strings.Contains(body, "Starting backend") ||
		!strings.Contains(body, "X-Preview-Poll") || strings.Contains(body, "http-equiv") {
		t.Fatalf("starting page: %d %v %q", code, hdr, body)
	}
}

func TestNonReadyStatuses(t *testing.T) {
	e := newTestEnv(t)
	d, err := e.db.CreateDeploy(e.repoID, shaOne, db.DeployMeta{})
	if err != nil {
		t.Fatal(err)
	}
	host := d.ShortSHA + "-demo.preview.localhost:8080"

	// Browsers get the polling interim page (marked with the interim header),
	// not a meta refresh — Firefox's autorefresh blocker prompts on those.
	code, body, hdr := doReq(t, e.router, host, "/", true)
	if code != 503 || hdr.Get("X-Preview-Interim") != "building" ||
		!strings.Contains(body, "X-Preview-Poll") || strings.Contains(body, "http-equiv") {
		t.Fatalf("queued page: %d %v %q", code, hdr, body)
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

	code, body, _ := doReq(t, e.router, "abcdef0-demo.preview.localhost", "/", true)
	if code != 404 || !strings.Contains(body, "Ambiguous") {
		t.Fatalf("ambiguous: %d %q", code, body)
	}
	// A full unique prefix works.
	code, _, _ = doReq(t, e.router, "abcdef01-demo.preview.localhost", "/", true)
	if code != 200 {
		t.Fatalf("unique prefix: %d", code)
	}

	code, body, _ = doReq(t, e.router, "1234567-nope.preview.localhost", "/", true)
	if code != 404 || !strings.Contains(body, "No repo") {
		t.Fatalf("unknown repo: %d %q", code, body)
	}
	code, body, _ = doReq(t, e.router, "fffffff-demo.preview.localhost", "/", true)
	if code != 404 || !strings.Contains(body, "No deploy") {
		t.Fatalf("unknown deploy: %d %q", code, body)
	}
	code, body, _ = doReq(t, e.router, "nothex-demo.preview.localhost", "/", true)
	if code != 404 || !strings.Contains(body, "not a preview address") {
		t.Fatalf("non-hex label: %d %q", code, body)
	}
}

func TestParseHost(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		host string
		sub  string
		ok   bool
	}{
		{"abc1234-demo.preview.localhost:8080", "abc1234-demo", true},
		{"ABC1234-DEMO.PREVIEW.LOCALHOST", "abc1234-demo", true},
		{"abc1234-demo.preview.localhost.", "abc1234-demo", true},
		{"preview.localhost", "", false},
		// A dotted host is no longer a preview address: one label only, so
		// that a single wildcard record and cert cover every repo.
		{"abc1234.demo.preview.localhost", "", false},
		{"a.b.c.preview.localhost", "", false},
		{"localhost:8080", "", false},
		{"[::1]:8080", "", false},
	}
	for _, tc := range cases {
		sub, ok := e.router.parseHost(tc.host)
		if sub != tc.sub || ok != tc.ok {
			t.Errorf("parseHost(%q) = (%q, %v), want (%q, %v)",
				tc.host, sub, ok, tc.sub, tc.ok)
		}
	}
}

// Repo names may contain hyphens, so the separator isn't always the first
// one: the split is resolved against the registry.
func TestHyphenatedRepoName(t *testing.T) {
	e := newTestEnv(t)
	if _, err := e.db.CreateRepo("my-app", "/src2", "/bare2", db.RepoReady); err != nil {
		t.Fatal(err)
	}
	label, repo, _, ok := e.router.splitSub("abc1234-my-app")
	if !ok || label != "abc1234" || repo.Name != "my-app" {
		t.Fatalf("splitSub = (%q, %q, %v), want (abc1234, my-app, true)", label, repo.Name, ok)
	}
	// The leftmost split whose left side is hex wins only if that repo
	// exists; "abc1234-my" is not registered, so it must not shadow it.
	if _, _, guess, ok := e.router.splitSub("abc1234-nosuch-repo"); ok || guess != "nosuch-repo" {
		t.Fatalf("unregistered: ok=%v guess=%q, want false/nosuch-repo", ok, guess)
	}
}

func cookieByName(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestPreviewAuthRedirectsWhenUnauthenticated(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	e.router.SetPreviewAuth(true, "http://localhost:8080", false)

	host := d.ShortSHA + "-demo.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://localhost:8080/api/auth/preview-grant?return_to=") {
		t.Fatalf("location %q", loc)
	}
}

func TestPreviewAuthValidCookieServes(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	e.router.SetPreviewAuth(true, "http://localhost:8080", false)

	raw, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.CreateSession(db.Session{
		TokenHash: hash, Scope: "preview", GitHubLogin: "octocat", GitHubUserID: 1,
	}, time.Hour); err != nil {
		t.Fatal(err)
	}

	host := d.ShortSHA + "-demo.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: previewGrantCookieName, Value: raw})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "preview home") {
		t.Fatalf("want served preview, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestPreviewAuthRedeemsCode(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	e.router.SetPreviewAuth(true, "http://localhost:8080", false)

	apex, err := e.db.CreateSession(db.Session{
		TokenHash: "apexhash", Scope: "apex", GitHubLogin: "octocat", GitHubUserID: 1,
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	code := "grantcode"
	if err := e.db.CreatePreviewGrant(hashToken(code), apex.ID, time.Minute); err != nil {
		t.Fatal(err)
	}

	host := d.ShortSHA + "-demo.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/?preview_auth="+code, nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	set := cookieByName(rec.Result().Cookies(), previewGrantCookieName)
	if set == nil || set.Value == "" {
		t.Fatal("expected preview_grant cookie to be set")
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "preview_auth") {
		t.Fatalf("redirect should strip the code, got %q", loc)
	}
	// The grant is consumed: a second redemption fails.
	if _, err := e.db.RedeemPreviewGrant(hashToken(code)); err == nil {
		t.Fatal("grant should be single-use")
	}
}

func TestPreviewGrantCookieStrippedFromBackend(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	e.router.SetPreviewAuth(true, "http://localhost:8080", false)

	var gotCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())
	e.fake.port = port

	raw, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.CreateSession(db.Session{
		TokenHash: hash, Scope: "preview", GitHubLogin: "octocat", GitHubUserID: 1,
	}, time.Hour); err != nil {
		t.Fatal(err)
	}

	host := d.ShortSHA + "-demo.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/api/things", nil)
	req.Host = host
	req.AddCookie(&http.Cookie{Name: previewGrantCookieName, Value: raw})
	req.AddCookie(&http.Cookie{Name: "keep", Value: "me"})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
	if strings.Contains(gotCookie, previewGrantCookieName) {
		t.Fatalf("backend must not receive the preview-access cookie: %q", gotCookie)
	}
	if !strings.Contains(gotCookie, "keep=me") {
		t.Fatalf("backend should still receive the app's own cookies: %q", gotCookie)
	}
}

// TestReservedUpstream: a reserved label is reverse-proxied wholesale to its
// upstream with no deploy row at all, every path passes through unchanged, the
// Host is rewritten to the upstream (real host in X-Forwarded-Host), and the
// domain-wide preview-access cookie is stripped while the app's own cookies
// survive.
func TestReservedUpstream(t *testing.T) {
	e := newTestEnv(t)

	var gotPath, gotHost, gotForwarded, gotCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost, gotForwarded = r.URL.Path, r.Host, r.Header.Get("X-Forwarded-Host")
		gotCookie = r.Header.Get("Cookie")
		fmt.Fprintf(w, "app:%s", r.URL.Path)
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	e.router.SetReservedUpstreams(map[string]string{"app": u.Host})

	host := "app.preview.localhost:8080"
	// A path that is neither /api nor a preview asset still reaches the upstream.
	req := httptest.NewRequest("GET", "http://"+host+"/auth/oauth/callback?x=1", nil)
	req.Host = host
	req.AddCookie(&http.Cookie{Name: previewGrantCookieName, Value: "secret"})
	req.AddCookie(&http.Cookie{Name: "fastapiusersauth", Value: "keep"})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != 200 || rec.Body.String() != "app:/auth/oauth/callback" {
		t.Fatalf("reserved proxy: %d %q", rec.Code, rec.Body.String())
	}
	if gotPath != "/auth/oauth/callback" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotHost != u.Host {
		t.Fatalf("upstream Host = %q, want %q", gotHost, u.Host)
	}
	if gotForwarded != host {
		t.Fatalf("X-Forwarded-Host = %q, want %q", gotForwarded, host)
	}
	if strings.Contains(gotCookie, previewGrantCookieName) {
		t.Fatalf("companion app must not receive the preview-access cookie: %q", gotCookie)
	}
	if !strings.Contains(gotCookie, "fastapiusersauth=keep") {
		t.Fatalf("companion app should keep its own cookie: %q", gotCookie)
	}
}

// TestReservedUpstreamBehindAuth: a reserved host is still gated by SSO — an
// unauthenticated request bounces to the grant handshake and never reaches the
// upstream.
func TestReservedUpstreamBehindAuth(t *testing.T) {
	e := newTestEnv(t)
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		fmt.Fprint(w, "app")
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	e.router.SetReservedUpstreams(map[string]string{"app": u.Host})
	e.router.SetPreviewAuth(true, "http://localhost:8080", false)

	host := "app.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302 to grant handshake, got %d", rec.Code)
	}
	if reached {
		t.Fatal("upstream must not be reached by an unauthenticated caller")
	}
}

// TestInterimAndErrorPagesAreUncacheable: interim states are moments, and an
// error page changes on redeploy — a cached copy of either would show
// "Building…" or "Build failed" long after reality moved on.
func TestInterimAndErrorPagesAreUncacheable(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	host := d.ShortSHA + "-demo.preview.localhost:8080"

	// Interim HTML page (browser, still starting).
	e.fake.slow = true
	req := httptest.NewRequest("GET", "http://"+host+"/api/x", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if cc := rec.Result().Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("interim page Cache-Control = %q, want no-store", cc)
	}

	// Poll JSON.
	_, _, hdr := doPoll(t, e.router, host, "/api/x", 0, 0)
	if cc := hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("poll Cache-Control = %q, want no-store", cc)
	}

	// Error page (unknown preview).
	req = httptest.NewRequest("GET", "http://nope-demo.preview.localhost:8080/", nil)
	req.Host = "nope-demo.preview.localhost:8080"
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if cc := rec.Result().Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("error page Cache-Control = %q, want no-store", cc)
	}
}

// TestOnyxAuthBouncesPreviewWithoutSession: a browser navigation to a preview
// with no onyx session cookie is 302'd to the canonical host's /auth/login, and
// a domain-scoped return marker is stashed so login can send it back.
func TestOnyxAuthBouncesPreviewWithoutSession(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	e.router.SetReservedUpstreams(map[string]string{"app": "127.0.0.1:9"})
	e.router.SetOnyxAuth("app", "")

	host := d.ShortSHA + "-demo.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/chat", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "http://app.preview.localhost/auth/login" {
		t.Fatalf("Location = %q", loc)
	}
	ret := cookieByName(rec.Result().Cookies(), onyxReturnCookieName)
	if ret == nil || ret.Value != "http://"+host+"/chat" {
		t.Fatalf("onyx_return = %+v, want the preview URL", ret)
	}
	// Go strips the leading dot when serializing Set-Cookie; a Domain-scoped
	// cookie is still presented to every subdomain (RFC 6265).
	if ret.Domain != "preview.localhost" {
		t.Fatalf("onyx_return Domain = %q, want preview.localhost", ret.Domain)
	}
}

// TestOnyxAuthPassesWithSession: once the browser holds an onyx cookie the
// request proceeds to the deploy (the preview's own onyx validates the JWT).
func TestOnyxAuthPassesWithSession(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	e.router.SetReservedUpstreams(map[string]string{"app": "127.0.0.1:9"})
	e.router.SetOnyxAuth("app", "")

	host := d.ShortSHA + "-demo.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "fastapiusersauth", Value: "jwt"})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "preview home") {
		t.Fatalf("want the static preview served, got %d %q", rec.Code, rec.Body.String())
	}
}

// TestOnyxAuthInterceptsLoginPage: the preview's own /auth/login is always
// bounced to the canonical host (single-tenant onyx would show a password form
// there, and an expired session redirects to it) — even with a cookie present —
// and the return marker points at the preview root, not back at /auth/login.
func TestOnyxAuthInterceptsLoginPage(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	e.router.SetReservedUpstreams(map[string]string{"app": "127.0.0.1:9"})
	e.router.SetOnyxAuth("app", "")

	host := d.ShortSHA + "-demo.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/auth/login", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "fastapiusersauth", Value: "stale"})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "http://app.preview.localhost/auth/login" {
		t.Fatalf("Location = %q", loc)
	}
	ret := cookieByName(rec.Result().Cookies(), onyxReturnCookieName)
	if ret == nil || ret.Value != "http://"+host+"/" {
		t.Fatalf("onyx_return = %+v, want the preview root", ret)
	}
}

// TestOnyxAuthNonBrowserFallsThrough: an XHR/fetch (no text/html) without a
// session is not redirected — it falls through to the preview's onyx, which
// 401s on its own. Here it reaches the (missing) backend and 502s, proving it
// was not bounced.
func TestOnyxAuthNonBrowserFallsThrough(t *testing.T) {
	e := newTestEnv(t)
	d := e.readyDeploy(t, shaOne)
	e.fake.err = errors.New("no backend")
	e.router.SetReservedUpstreams(map[string]string{"app": "127.0.0.1:9"})
	e.router.SetOnyxAuth("app", "")

	host := d.ShortSHA + "-demo.preview.localhost:8080"
	code, _, _ := doReq(t, e.router, host, "/api/me", false)
	if code == http.StatusFound {
		t.Fatal("a non-browser request must not be bounced to onyx login")
	}
}

// TestOnyxAuthPostLoginHandoff: back on the canonical host with a fresh session
// and a stashed return address, the browser is sent to the preview it came
// from and the marker is cleared.
func TestOnyxAuthPostLoginHandoff(t *testing.T) {
	e := newTestEnv(t)
	e.router.SetReservedUpstreams(map[string]string{"app": "127.0.0.1:9"})
	e.router.SetOnyxAuth("app", "")

	host := "app.preview.localhost:8080"
	ret := "http://1a2b3c4-demo.preview.localhost:8080/chat"
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "fastapiusersauth", Value: "jwt"})
	req.AddCookie(&http.Cookie{Name: onyxReturnCookieName, Value: ret})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != ret {
		t.Fatalf("Location = %q, want %q", loc, ret)
	}
	if c := cookieByName(rec.Result().Cookies(), onyxReturnCookieName); c == nil || c.MaxAge >= 0 {
		t.Fatalf("onyx_return should be cleared, got %+v", c)
	}
	// The dance must also hand the browser a preview-domain copy of the onyx
	// session — otherwise the host-only canonical cookie never reaches the
	// preview, needsOnyxLogin bounces the browser right back here, and the
	// user loops between the preview and the canonical login forever.
	auth := cookieByName(rec.Result().Cookies(), "fastapiusersauth")
	if auth == nil || auth.Value != "jwt" {
		t.Fatalf("expected widened onyx session cookie, got %+v", auth)
	}
	if auth.Domain != "preview.localhost" {
		t.Fatalf("widened session Domain = %q, want preview.localhost", auth.Domain)
	}
	if !auth.HttpOnly || auth.MaxAge <= 0 {
		t.Fatalf("widened session must be HttpOnly and persistent, got %+v", auth)
	}
}

// TestOnyxAuthRejectsForeignReturn: a return marker pointing off our domain (a
// malicious previewed backend could set the domain-scoped cookie) is ignored —
// no open redirect.
func TestOnyxAuthRejectsForeignReturn(t *testing.T) {
	e := newTestEnv(t)
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		fmt.Fprint(w, "app-home")
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	e.router.SetReservedUpstreams(map[string]string{"app": u.Host})
	e.router.SetOnyxAuth("app", "")

	host := "app.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	req.Host = host
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: "fastapiusersauth", Value: "jwt"})
	req.AddCookie(&http.Cookie{Name: onyxReturnCookieName, Value: "http://evil.com/steal"})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	if rec.Code == http.StatusFound {
		t.Fatalf("must not redirect to a foreign return, got Location %q", rec.Header().Get("Location"))
	}
	if !reached {
		t.Fatal("expected the request to fall through to the canonical app")
	}
}

// TestOnyxAuthCookieDomainRewrite: the onyx session cookie the canonical host
// sets host-only is widened to the whole preview domain on the way out, so the
// browser presents it to every preview subdomain.
func TestOnyxAuthCookieDomainRewrite(t *testing.T) {
	e := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "fastapiusersauth", Value: "jwt", Path: "/", HttpOnly: true})
		http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: "c", Path: "/"})
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	e.router.SetReservedUpstreams(map[string]string{"app": u.Host})
	e.router.SetOnyxAuth("app", "")

	host := "app.preview.localhost:8080"
	req := httptest.NewRequest("GET", "http://"+host+"/api/auth/oauth/callback", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)

	auth := cookieByName(rec.Result().Cookies(), "fastapiusersauth")
	if auth == nil || auth.Domain != "preview.localhost" {
		t.Fatalf("auth cookie Domain = %+v, want preview.localhost", auth)
	}
	if csrf := cookieByName(rec.Result().Cookies(), "csrftoken"); csrf == nil || csrf.Domain != "" {
		t.Fatalf("csrf cookie should stay host-only, got %+v", csrf)
	}
}
