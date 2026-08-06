// Package proxy is the top-level request router. Hosts under the preview
// domain (<label>.<repo>.<domain>) serve deployed previews — static frontend
// files plus /api/* reverse-proxied to the supervised backend. Every other
// host (the apex domain, localhost, raw IPs) gets the dashboard.
//
// Routing state is cached in memory with a short TTL so the single-connection
// SQLite database isn't queried for every asset request.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

// coldStartWait is how long a request waits for a backend before returning
// the interim "starting" response. The start itself continues in the
// background either way.
const coldStartWait = 1500 * time.Millisecond

// cacheTTL bounds staleness of the routing cache; preview status pages
// refresh on the same order, so transitions surface promptly.
const cacheTTL = 2 * time.Second

// Backends is the slice of the supervisor the proxy needs.
type Backends interface {
	EnsureRunning(ctx context.Context, k supervise.Key, repoName string) (int, error)
}

// Router routes by Host header. It wraps the dashboard handler and serves
// previews itself.
type Router struct {
	db        *db.Store
	files     *store.Store
	backends  Backends
	domain    string
	dashboard http.Handler

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	repoID    int64
	deploy    db.Deploy
	err       string // non-empty: resolution failed with this message
	ambiguous []string
	expires   time.Time

	// Routing shape of a ready deploy, resolved once per cache fill.
	feProcess   bool     // frontend is a supervised process, not a static dist
	stripAPI    bool     // remove the /api prefix before proxying
	extraRoutes []string // additional unstripped prefixes routed to the backend
}

// New returns a Router serving previews under domain and everything else
// from dashboard.
func New(database *db.Store, files *store.Store, backends Backends, domain string, dashboard http.Handler) *Router {
	return &Router{
		db:        database,
		files:     files,
		backends:  backends,
		domain:    strings.ToLower(domain),
		dashboard: dashboard,
		cache:     make(map[string]cacheEntry),
	}
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	label, repoName, ok := rt.parseHost(r.Host)
	if !ok {
		rt.dashboard.ServeHTTP(w, r)
		return
	}
	entry := rt.resolve(label, repoName)
	switch {
	case len(entry.ambiguous) > 0:
		rt.errorPage(w, r, http.StatusNotFound, "Ambiguous preview address",
			fmt.Sprintf("%q matches several deploys (%s) — use a longer sha prefix.",
				label, strings.Join(entry.ambiguous, ", ")))
	case entry.err != "":
		rt.errorPage(w, r, http.StatusNotFound, "Unknown preview", entry.err)
	default:
		rt.servePreview(w, r, entry)
	}
}

// parseHost splits a request host into (label, repo) if it's a preview
// host. ok=false means "not a preview host" → dashboard.
func (rt *Router) parseHost(hostport string) (label, repo string, ok bool) {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	rest, found := strings.CutSuffix(host, "."+rt.domain)
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// resolve maps (label, repo) to a deploy through the TTL cache.
func (rt *Router) resolve(label, repoName string) cacheEntry {
	key := repoName + "\x00" + label
	rt.mu.Lock()
	if e, ok := rt.cache[key]; ok && time.Now().Before(e.expires) {
		rt.mu.Unlock()
		return e
	}
	rt.mu.Unlock()

	e := rt.lookup(label, repoName)
	e.expires = time.Now().Add(cacheTTL)
	rt.mu.Lock()
	rt.cache[key] = e
	rt.mu.Unlock()
	return e
}

func (rt *Router) lookup(label, repoName string) cacheEntry {
	repo, err := rt.db.GetRepoByName(repoName)
	if err != nil {
		return cacheEntry{err: fmt.Sprintf("No repo named %q is registered.", repoName)}
	}
	if !isHex(label) {
		// Branch aliases arrive in M2; today labels are sha prefixes.
		return cacheEntry{err: fmt.Sprintf("%q is not a sha prefix.", label)}
	}
	matches, err := rt.db.DeploysBySHAPrefix(repo.ID, label)
	if err != nil || len(matches) == 0 {
		return cacheEntry{err: fmt.Sprintf("No deploy of %s matches %q. Deploy it with: preview deploy %s", repoName, label, label)}
	}
	// Distinct shas → ambiguous. (The same sha can't appear twice per repo.)
	if len(matches) > 1 {
		shorts := make([]string, len(matches))
		for i, m := range matches {
			shorts[i] = m.ShortSHA
		}
		return cacheEntry{ambiguous: shorts}
	}
	e := cacheEntry{repoID: repo.ID, deploy: matches[0]}
	if e.deploy.Status == db.DeployReady {
		if e.deploy.FeHash != "" {
			if _, err := rt.db.GetFrontendArtifact(repo.ID, e.deploy.FeHash); err == nil {
				e.feProcess = true
			}
		}
		if art, err := rt.db.GetBackendArtifact(repo.ID, e.deploy.BeHash); err == nil {
			var cfg struct {
				StripAPIPrefix bool     `json:"strip_api_prefix"`
				ExtraRoutes    []string `json:"extra_routes"`
			}
			if json.Unmarshal([]byte(art.RunConfig), &cfg) == nil {
				e.stripAPI = cfg.StripAPIPrefix
				e.extraRoutes = cfg.ExtraRoutes
			}
		}
	}
	return e
}

func (rt *Router) servePreview(w http.ResponseWriter, r *http.Request, e cacheEntry) {
	d := e.deploy
	repoName := rt.repoNameFromHost(r.Host)
	switch d.Status {
	case db.DeployQueued, db.DeployBuilding:
		rt.refreshPage(w, r, http.StatusServiceUnavailable, "Building preview…",
			fmt.Sprintf("Deploy %s of %s is %s. This page refreshes automatically.", d.ShortSHA, repoName, d.Status))
	case db.DeployFailed:
		rt.errorPage(w, r, http.StatusBadGateway, "Build failed",
			fmt.Sprintf("Deploy %s failed: %s (full logs: preview deploy logs %d)", d.ShortSHA, d.Error, d.ID))
	case db.DeployEvicted:
		rt.errorPage(w, r, http.StatusGone, "Preview cleaned up",
			fmt.Sprintf("Deploy %s was garbage-collected. Redeploy it with: preview deploy %s", d.ShortSHA, d.SHA))
	case db.DeployReady:
		if strings.HasPrefix(r.URL.Path, "/api/") || matchesRoute(e.extraRoutes, r.URL.Path) {
			rt.proxyAPI(w, r, e, repoName)
			return
		}
		if e.feProcess {
			rt.proxyFrontend(w, r, e, repoName)
			return
		}
		rt.serveStatic(w, r, repoName, d.FeHash)
	default:
		rt.errorPage(w, r, http.StatusInternalServerError, "Unknown state", d.Status)
	}
}

func (rt *Router) repoNameFromHost(host string) string {
	_, repo, _ := rt.parseHost(host)
	return repo
}

// proxyAPI routes backend traffic, stripping the /api prefix when the
// manifest asks for it (extra routes are never stripped).
func (rt *Router) proxyAPI(w http.ResponseWriter, r *http.Request, e cacheEntry, repoName string) {
	if e.stripAPI && strings.HasPrefix(r.URL.Path, "/api/") {
		r = r.Clone(r.Context())
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
	}
	rt.ensureAndProxy(w, r, e, repoName, supervise.BackendKey(e.repoID, e.deploy.BeHash), "backend")
}

// proxyFrontend routes page traffic to a process-mode frontend.
func (rt *Router) proxyFrontend(w http.ResponseWriter, r *http.Request, e cacheEntry, repoName string) {
	rt.ensureAndProxy(w, r, e, repoName,
		supervise.FrontendKey(e.repoID, e.deploy.FeHash, e.deploy.BeHash), "frontend")
}

// ensureAndProxy cold-starts the keyed process if needed (bounded wait; the
// start continues in the background) and reverse-proxies to it.
func (rt *Router) ensureAndProxy(w http.ResponseWriter, r *http.Request, e cacheEntry, repoName string, k supervise.Key, what string) {
	ctx, cancel := context.WithTimeout(r.Context(), coldStartWait)
	defer cancel()
	port, err := rt.backends.EnsureRunning(ctx, k, repoName)
	if err != nil {
		if ctx.Err() != nil && r.Context().Err() == nil {
			// Still starting — tell the client to come back shortly.
			w.Header().Set("Retry-After", "2")
			rt.refreshPage(w, r, http.StatusServiceUnavailable, "Starting "+what+"…",
				"The preview "+what+" is starting. This page refreshes automatically.")
			return
		}
		rt.errorPage(w, r, http.StatusBadGateway, "Preview unavailable",
			fmt.Sprintf("The preview %s failed to start: %s (run logs: preview deploy logs %d)", what, err, e.deploy.ID))
		return
	}
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	proxy := &httputil.ReverseProxy{
		// SetURL also rewrites the outbound Host to the target: backends see
		// themselves, not the preview subdomain (which would confuse any app
		// that routes on Host — including a nested local-preview). The
		// original host travels in X-Forwarded-Host.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rt.errorPage(w, r, http.StatusBadGateway, "Preview error", err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

// matchesRoute reports whether path falls under any of the prefixes.
func matchesRoute(routes []string, path string) bool {
	for _, route := range routes {
		if path == route || strings.HasPrefix(path, route+"/") {
			return true
		}
	}
	return false
}

// serveStatic serves the frontend artifact with SPA fallback, mirroring the
// embedded-dashboard handler's behavior but rooted at a disk directory.
func (rt *Router) serveStatic(w http.ResponseWriter, r *http.Request, repoName, feHash string) {
	root := http.Dir(rt.files.FrontendDir(repoName, feHash))
	fileServer := http.FileServer(root)
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		fileServer.ServeHTTP(w, r)
		return
	}
	if f, err := root.Open(p); err != nil {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
		return
	} else {
		f.Close()
	}
	fileServer.ServeHTTP(w, r)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// wantsHTML sniffs whether the client is a browser navigation (serve an
// HTML status page) or an API/fetch caller (serve JSON).
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func (rt *Router) errorPage(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	rt.statusResponse(w, r, status, title, detail, false)
}

func (rt *Router) refreshPage(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	rt.statusResponse(w, r, status, title, detail, true)
}

func (rt *Router) statusResponse(w http.ResponseWriter, r *http.Request, status int, title, detail string, refresh bool) {
	if !wantsHTML(r) {
		w.Header().Set("Content-Type", "application/json")
		if refresh {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":%q}`+"\n", title+": "+detail)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	meta := ""
	if refresh {
		meta = `<meta http-equiv="refresh" content="2">`
	}
	fmt.Fprintf(w, `<!doctype html><html><head><title>%s</title>%s
<style>body{font-family:system-ui,sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center;background:#111;color:#eee}
main{max-width:36rem;padding:2rem}h1{font-size:1.25rem}p{color:#aaa;line-height:1.5}</style>
</head><body><main><h1>%s</h1><p>%s</p></main></body></html>
`, html.EscapeString(title), meta, html.EscapeString(title), html.EscapeString(detail))
}
