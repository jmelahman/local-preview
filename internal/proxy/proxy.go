// Package proxy is the top-level request router. Hosts under the preview
// domain (<label>-<repo>.<domain>) serve deployed previews — static frontend
// files plus /api/* reverse-proxied to the supervised backend. Every other
// host (the apex domain, localhost, raw IPs) gets the dashboard.
//
// Preview hosts must stay a single DNS label: a wildcard matches one label
// only, so this is what lets one *.<domain> record and one wildcard
// certificate cover every repo.
//
// Routing state is cached in memory with a short TTL so the single-connection
// SQLite database isn't queried for every asset request.
package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// previewSessionTTL is how long a preview-access session lasts before the
// browser must re-run the apex→preview handshake.
const previewSessionTTL = 7 * 24 * time.Hour

// previewGrantCookieName is the preview-scoped session cookie the proxy sets
// after redeeming a grant. It is deliberately distinct from the apex dashboard
// cookie (internal/api's "preview_session"): a preview subdomain runs
// untrusted code and must never hold the dashboard credential.
const previewGrantCookieName = "preview_grant"

// previewAuthParam is the one-time handoff code the apex redirects back with.
const previewAuthParam = "preview_auth"

// Backends is the slice of the supervisor the proxy needs. EnsureRunning
// returns the "host:port" the started process serves on — loopback for a
// process on this node (single-node / worker-local), or a worker's address when
// the control node routes to an elastic worker tier. The proxy is transport-
// agnostic: the same interface is satisfied by a local Manager adapter and by a
// remote worker-API client.
type Backends interface {
	EnsureRunning(ctx context.Context, k supervise.Key, repoName string) (addr string, err error)
}

// Router routes by Host header. It wraps the dashboard handler and serves
// previews itself.
type Router struct {
	db        *db.Store
	files     *store.Store
	backends  Backends
	domain    string
	dashboard http.Handler

	// authEnabled gates preview subdomains behind the SSO login; when off the
	// Router behaves exactly as before. authBaseURL is the dashboard origin the
	// grant handshake bounces through; authSecure marks the preview cookie
	// Secure.
	authEnabled bool
	authBaseURL string
	authSecure  bool

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	repoID    int64
	repoName  string
	label     string // sha prefix the host asked for
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

// SetPreviewAuth turns on SSO gating of preview subdomains. baseURL is the
// dashboard origin (scheme://host) the grant handshake redirects through, and
// secure marks the preview cookie Secure. The zero value (never calling this)
// leaves previews open, so existing callers and tests are unaffected.
func (rt *Router) SetPreviewAuth(enabled bool, baseURL string, secure bool) {
	rt.authEnabled = enabled
	rt.authBaseURL = strings.TrimRight(baseURL, "/")
	rt.authSecure = secure
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub, ok := rt.parseHost(r.Host)
	if !ok {
		rt.dashboard.ServeHTTP(w, r)
		return
	}
	// Gate preview traffic before any DB lookup, so an unauthenticated caller
	// can't even probe which previews exist.
	if rt.authEnabled && !rt.previewAuthorized(w, r) {
		return
	}
	entry := rt.resolve(sub)
	switch {
	case len(entry.ambiguous) > 0:
		rt.errorPage(w, r, http.StatusNotFound, "Ambiguous preview address",
			fmt.Sprintf("%q matches several deploys (%s) — use a longer sha prefix.",
				entry.label, strings.Join(entry.ambiguous, ", ")))
	case entry.err != "":
		rt.errorPage(w, r, http.StatusNotFound, "Unknown preview", entry.err)
	default:
		rt.servePreview(w, r, entry)
	}
}

// previewAuthorized authorizes a preview request. It redeems a one-time
// ?preview_auth code into a preview-scoped cookie, accepts an existing valid
// preview cookie, or bounces the browser through the dashboard's grant
// handshake. It returns true only when the request may proceed; otherwise it
// has already written a redirect and the caller must stop.
func (rt *Router) previewAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if code := r.URL.Query().Get(previewAuthParam); code != "" {
		return rt.redeemPreviewGrant(w, r, code)
	}
	if c, err := r.Cookie(previewGrantCookieName); err == nil {
		if _, err := rt.db.GetSessionByTokenHash("preview", hashToken(c.Value)); err == nil {
			return true
		}
	}
	rt.redirectToGrant(w, r)
	return false
}

// redeemPreviewGrant consumes a handoff code and, on success, establishes a
// preview-scoped session (copying the apex session's identity) and sets its
// cookie for the whole preview domain, then redirects to the clean URL. Any
// failure restarts the handshake rather than leaking a reason.
func (rt *Router) redeemPreviewGrant(w http.ResponseWriter, r *http.Request, code string) bool {
	apexID, err := rt.db.RedeemPreviewGrant(hashToken(code))
	if err != nil {
		rt.redirectToGrant(w, r)
		return false
	}
	apex, err := rt.db.GetSessionByID(apexID)
	if err != nil {
		rt.redirectToGrant(w, r)
		return false
	}
	raw, hash, err := newToken()
	if err != nil {
		rt.errorPage(w, r, http.StatusInternalServerError, "Preview auth error",
			"Could not establish a preview session. Try again.")
		return false
	}
	if _, err := rt.db.CreateSession(db.Session{
		TokenHash:    hash,
		Scope:        "preview",
		GitHubLogin:  apex.GitHubLogin,
		GitHubUserID: apex.GitHubUserID,
		Email:        apex.Email,
		AvatarURL:    apex.AvatarURL,
	}, previewSessionTTL); err != nil {
		rt.errorPage(w, r, http.StatusInternalServerError, "Preview auth error",
			"Could not establish a preview session. Try again.")
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     previewGrantCookieName,
		Value:    raw,
		Path:     "/",
		Domain:   "." + rt.domain,
		HttpOnly: true,
		Secure:   rt.authSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(previewSessionTTL),
		MaxAge:   int(previewSessionTTL.Seconds()),
	})
	http.Redirect(w, r, previewReturnURL(r), http.StatusFound)
	return false
}

// redirectToGrant sends the browser to the dashboard's preview-grant endpoint,
// which mints a code (after logging the user in if needed) and redirects back.
func (rt *Router) redirectToGrant(w http.ResponseWriter, r *http.Request) {
	target := rt.authBaseURL + "/api/auth/preview-grant?return_to=" +
		url.QueryEscape(previewReturnURL(r))
	http.Redirect(w, r, target, http.StatusFound)
}

// previewReturnURL is the absolute URL of the current preview request with the
// one-time code stripped — where the handshake should land the browser.
func previewReturnURL(r *http.Request) string {
	u := *r.URL
	q := u.Query()
	q.Del(previewAuthParam)
	u.RawQuery = q.Encode()
	u.Scheme = requestScheme(r)
	u.Host = r.Host
	return u.String()
}

// requestScheme reports the public scheme, honoring a TLS-terminating proxy's
// X-Forwarded-Proto before the local connection's TLS state.
func requestScheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// newToken returns a 256-bit random token and its sha256 hex hash. Only the
// hash is persisted; the raw value lives in the cookie.
func newToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashToken(raw), nil
}

// hashToken maps a raw token to its stored form.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// stripCookie removes a single named cookie from a request's Cookie header,
// leaving the others intact.
func stripCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == name {
			continue
		}
		r.AddCookie(c)
	}
}

// parseHost returns the single label below the preview domain, unsplit —
// telling <label>-<repo> apart needs the repo registry, which lookup has.
// ok=false means "not a preview host" → dashboard.
func (rt *Router) parseHost(hostport string) (sub string, ok bool) {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	rest, found := strings.CutSuffix(host, "."+rt.domain)
	if !found {
		return "", false
	}
	// One label only; a deeper host isn't ours.
	if rest == "" || strings.Contains(rest, ".") {
		return "", false
	}
	return rest, true
}

// resolve maps a preview label to a deploy through the TTL cache.
func (rt *Router) resolve(sub string) cacheEntry {
	key := sub
	rt.mu.Lock()
	if e, ok := rt.cache[key]; ok && time.Now().Before(e.expires) {
		rt.mu.Unlock()
		return e
	}
	rt.mu.Unlock()

	e := rt.lookup(sub)
	e.expires = time.Now().Add(cacheTTL)
	rt.mu.Lock()
	rt.cache[key] = e
	rt.mu.Unlock()
	return e
}

// splitSub resolves <label>-<repo> against the repo registry. Repo names may
// themselves contain hyphens, so the first hyphen isn't necessarily the
// separator: try every split whose left side is a sha prefix and take the
// first one naming a registered repo. guess is the leftmost candidate's repo,
// used only to word the error when nothing matches.
func (rt *Router) splitSub(sub string) (label string, repo db.Repo, guess string, ok bool) {
	for i, c := range sub {
		if c != '-' {
			continue
		}
		left, right := sub[:i], sub[i+1:]
		if left == "" || right == "" || !isHex(left) {
			continue
		}
		if guess == "" {
			guess = right
		}
		r, err := rt.db.GetRepoByName(right)
		if err != nil {
			continue
		}
		return left, r, guess, true
	}
	return "", db.Repo{}, guess, false
}

func (rt *Router) lookup(sub string) cacheEntry {
	label, repo, guess, ok := rt.splitSub(sub)
	if !ok {
		if guess == "" {
			// No hyphen split had a sha prefix on the left, so the host is
			// the wrong shape rather than naming an unknown repo.
			return cacheEntry{err: fmt.Sprintf("%q is not a preview address (expected <sha>-<repo>).", sub)}
		}
		return cacheEntry{err: fmt.Sprintf("No repo named %q is registered.", guess)}
	}
	repoName := repo.Name
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
		return cacheEntry{repoName: repoName, label: label, ambiguous: shorts}
	}
	e := cacheEntry{repoID: repo.ID, repoName: repoName, label: label, deploy: matches[0]}
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
	repoName := e.repoName
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
	addr, err := rt.backends.EnsureRunning(ctx, k, repoName)
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
	target := &url.URL{Scheme: "http", Host: addr}
	proxy := &httputil.ReverseProxy{
		// SetURL also rewrites the outbound Host to the target: backends see
		// themselves, not the preview subdomain (which would confuse any app
		// that routes on Host — including a nested local-preview). The
		// original host travels in X-Forwarded-Host.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			// Never hand the orchestrator's preview-access credential to the
			// previewed (untrusted) app — httputil forwards Cookie verbatim
			// otherwise, letting a malicious backend replay it against the API.
			stripCookie(pr.Out, previewGrantCookieName)
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
	// A static frontend has no supervised process, so nothing on the serving
	// path would otherwise hydrate it — yet with a durable tier its local files
	// are a cache that the sweeper can reclaim while the deploy stays ready.
	// Bring it back here, mirroring supervise.start's hydrate-on-serve: absence
	// of files is a cache miss to refill, not an eviction to 404. (No tier =
	// today's behavior exactly: local disk is authoritative, so skip the dance.)
	if rt.files.ArtifactTier() != nil {
		if !rt.files.HasFrontend(repoName, feHash) {
			ctx, cancel := context.WithTimeout(r.Context(), coldStartWait)
			defer cancel()
			if err := rt.files.Hydrate(ctx, repoName, "fe", feHash); err != nil {
				switch {
				case errors.Is(err, store.ErrNotInTier):
					rt.errorPage(w, r, http.StatusBadGateway, "Preview unavailable",
						"The preview frontend is missing from durable storage. Redeploy to rebuild it.")
				case ctx.Err() != nil && r.Context().Err() == nil:
					// Still landing — tell the client to come back shortly.
					w.Header().Set("Retry-After", "2")
					rt.refreshPage(w, r, http.StatusServiceUnavailable, "Loading preview…",
						"The preview frontend is being restored from durable storage. This page refreshes automatically.")
				default:
					rt.errorPage(w, r, http.StatusBadGateway, "Preview unavailable",
						"The preview frontend could not be restored: "+err.Error())
				}
				return
			}
		}
		// Mark it hot on every served request so a busy static frontend doesn't
		// sort coldest (its dir mtime is pinned at publish time) and get evicted
		// out from under live traffic.
		rt.files.NoteAccess(repoName, "fe", feHash)
	}

	// Explicit revalidation instead of heuristic freshness: previews are
	// content-addressed, but the URL only carries the commit sha — a
	// --rebuild of the same sha can change file contents under the same
	// URL, so nothing here may be marked immutable (target repos also
	// aren't guaranteed to content-hash their asset names). no-cache keeps
	// every response a cheap 304 via the files' Last-Modified until the
	// artifact really changes.
	w.Header().Set("Cache-Control", "no-cache")
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
