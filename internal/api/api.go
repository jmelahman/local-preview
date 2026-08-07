// Package api defines the HTTP surface served at the apex host: JSON
// endpoints under /api/ plus the embedded dashboard SPA at /.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/config"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/retain"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
	"github.com/jmelahman/local-preview/internal/watch"
	"github.com/jmelahman/local-preview/web"
)

// BuildInfo describes the running binary; mirrored from cmd/server so the
// api package doesn't import it.
type BuildInfo struct {
	Version string `json:"version"`
}

// Deps carries the dependencies handlers need.
type Deps struct {
	Store  *db.Store
	Build  BuildInfo
	Config config.Config
	Git    *gitrepo.Manager
	Queue  *build.Queue
	Super  *supervise.Manager
	// Watcher, when set, is kicked after watch settings change so a newly
	// watched repo polls immediately instead of waiting an interval.
	Watcher *watch.Watcher
	// Files locates published artifacts on disk (downloadable-artifact
	// listings and downloads).
	Files *store.Store
	// Sweeper runs retention sweeps (POST /api/gc).
	Sweeper *retain.Sweeper
	// DBPath is the SQLite location actually opened (":memory:" for
	// --in-memory), not Config's default — storage reporting sizes the real
	// database, never a stale file from a previous on-disk run.
	DBPath string
	// GitHubWebhookSecret validates X-Hub-Signature-256 on webhook
	// deliveries; empty disables POST /api/webhooks/github.
	GitHubWebhookSecret string
	// Addr is the server's listen address, used to construct preview URLs.
	Addr string
}

// NewMux returns the apex-host handler: API routes plus the dashboard SPA.
func NewMux(d Deps) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", d.handleHealth)
	mux.HandleFunc("POST /api/repos", d.handleCreateRepo)
	mux.HandleFunc("GET /api/repos", d.handleListRepos)
	mux.HandleFunc("GET /api/repos/{name}", d.handleGetRepo)
	mux.HandleFunc("PATCH /api/repos/{name}", d.handleUpdateRepo)
	mux.HandleFunc("DELETE /api/repos/{name}", d.handleDeleteRepo)
	mux.HandleFunc("POST /api/webhooks/github", d.handleGitHubWebhook)
	mux.HandleFunc("POST /api/deploys", d.handleCreateDeploy)
	mux.HandleFunc("GET /api/deploys", d.handleListDeploys)
	mux.HandleFunc("GET /api/deploys/{id}", d.handleGetDeploy)
	mux.HandleFunc("POST /api/deploys/{id}/stop", d.handleStopDeploy)
	mux.HandleFunc("DELETE /api/deploys/{id}", d.handleDeleteDeploy)
	mux.HandleFunc("GET /api/deploys/{id}/logs", d.handleDeployLogs)
	mux.HandleFunc("GET /api/deploys/{id}/logs/run", d.handleDeployRunLog)
	mux.HandleFunc("GET /api/deploys/{id}/stats", d.handleDeployStats)
	mux.HandleFunc("GET /api/deploys/{id}/artifacts/{name}/{file}", d.handleArtifactDownload)
	mux.HandleFunc("GET /api/storage", d.handleStorage)
	mux.HandleFunc("GET /api/retention", d.handleGetRetention)
	mux.HandleFunc("PUT /api/retention", d.handlePutRetention)
	mux.HandleFunc("POST /api/gc", d.handleRunGC)
	mux.Handle("/", web.Handler())
	return mux
}

// handleHealth reports liveness plus the runtime facts the dashboard can't
// know on its own — notably the preview base domain, which is fixed at
// startup by --preview-domain/$PREVIEW_DOMAIN.
func (d Deps) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":         "ok",
		"version":        d.Build.Version,
		"preview_domain": d.Config.PreviewDomain,
	})
}

func (d Deps) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		Source        string `json:"source"`
		Watch         bool   `json:"watch"`
		WatchBranches string `json:"watch_branches"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Source = strings.TrimSpace(req.Source)
	if err := gitrepo.ValidateName(req.Name); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Source == "" {
		httpError(w, http.StatusBadRequest, "source is required (a local path or clone URL)")
		return
	}
	branches, err := watch.ValidatePatterns(req.WatchBranches)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := d.Store.GetRepoByName(req.Name); err == nil {
		httpError(w, http.StatusConflict, fmt.Sprintf("repo %q already exists", req.Name))
		return
	}
	gr, err := d.Git.Add(r.Context(), req.Name, req.Source)
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("clone failed: %v", err))
		return
	}
	repo, err := d.Store.CreateRepo(req.Name, req.Source, gr.Path)
	if errors.Is(err, db.ErrConflict) {
		httpError(w, http.StatusConflict, fmt.Sprintf("repo %q already exists", req.Name))
		return
	}
	if err != nil {
		internalError(w, "create repo", err)
		return
	}
	if req.Watch || branches != "" {
		if repo, err = d.Store.SetRepoWatch(repo.ID, req.Watch, branches); err != nil {
			internalError(w, "set repo watch", err)
			return
		}
		if req.Watch {
			d.Watcher.Kick()
		}
	}
	writeJSON(w, http.StatusCreated, repo)
}

// handleUpdateRepo changes a repo's watch settings. Fields absent from the
// PATCH body keep their current value.
func (d Deps) handleUpdateRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := d.Store.GetRepoByName(r.PathValue("name"))
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "repo not found")
		return
	}
	if err != nil {
		internalError(w, "get repo", err)
		return
	}
	var req struct {
		Watch         *bool   `json:"watch"`
		WatchBranches *string `json:"watch_branches"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Watch == nil && req.WatchBranches == nil {
		httpError(w, http.StatusBadRequest, "nothing to update: set watch and/or watch_branches")
		return
	}
	watchOn, branches := repo.Watch, repo.WatchBranches
	if req.Watch != nil {
		watchOn = *req.Watch
	}
	if req.WatchBranches != nil {
		if branches, err = watch.ValidatePatterns(*req.WatchBranches); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	repo, err = d.Store.SetRepoWatch(repo.ID, watchOn, branches)
	if err != nil {
		internalError(w, "set repo watch", err)
		return
	}
	if watchOn {
		d.Watcher.Kick()
	}
	writeJSON(w, http.StatusOK, repo)
}

func (d Deps) handleListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := d.Store.ListRepos()
	if err != nil {
		internalError(w, "list repos", err)
		return
	}
	writeJSON(w, http.StatusOK, repos)
}

func (d Deps) handleGetRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := d.Store.GetRepoByName(r.PathValue("name"))
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "repo not found")
		return
	}
	if err != nil {
		internalError(w, "get repo", err)
		return
	}
	writeJSON(w, http.StatusOK, repo)
}

// handleDeleteRepo unregisters a repo: stops its backends, removes its DB
// rows, then deletes its mirror clone, artifacts, state, and logs. On-disk
// cleanup is best-effort once the rows are gone — leftovers are unreachable
// and only cost disk, so failures are logged rather than surfaced.
func (d Deps) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := d.Store.GetRepoByName(r.PathValue("name"))
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "repo not found")
		return
	}
	if err != nil {
		internalError(w, "get repo", err)
		return
	}
	d.Super.StopRepo(repo.ID, "repo deleted")
	d.Super.PurgeRepoContainers(repo.Name)
	if err := d.Store.DeleteRepo(repo.ID); err != nil {
		internalError(w, "delete repo", err)
		return
	}
	if err := d.Git.Remove(repo.Name); err != nil {
		log.Printf("delete repo %s: remove mirror: %v", repo.Name, err)
	}
	for _, dir := range []string{
		filepath.Join(d.Config.ArtifactsDir(), repo.Name),
		filepath.Join(d.Config.StateDir(), repo.Name),
		filepath.Join(d.Config.LogsDir(), repo.Name),
	} {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("delete repo %s: %v", repo.Name, err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// deployJSON augments a deploy row with its preview URL and the
// supervisor's live process statuses. FeProcess is present only for
// process-mode frontends; Artifacts only for manifests that declare
// downloadable artifacts.
type deployJSON struct {
	db.DeployRow
	PreviewURL string         `json:"preview_url,omitempty"`
	Process    string         `json:"process,omitempty"`
	FeProcess  string         `json:"fe_process,omitempty"`
	Artifacts  []artifactJSON `json:"artifacts,omitempty"`
}

// artifactJSON is one named downloadable artifact on a ready deploy.
type artifactJSON struct {
	Name  string             `json:"name"`
	Hash  string             `json:"hash"`
	Files []artifactFileJSON `json:"files"`
}

type artifactFileJSON struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// URL is the download path on the apex host.
	URL string `json:"url"`
}

func (d Deps) deployJSON(row db.DeployRow) deployJSON {
	out := deployJSON{DeployRow: row}
	if row.Status == db.DeployReady {
		out.PreviewURL = d.previewURL(row)
		if row.BeHash != "" {
			out.Process = d.Super.Status(supervise.BackendKey(row.RepoID, row.BeHash))
		}
		if row.FeHash != "" {
			if _, err := d.Store.GetFrontendArtifact(row.RepoID, row.FeHash); err == nil {
				out.FeProcess = d.Super.Status(supervise.FrontendKey(row.RepoID, row.FeHash, row.BeHash))
			}
		}
		for _, name := range slices.Sorted(maps.Keys(row.Artifacts)) {
			ref := row.Artifacts[name]
			art := artifactJSON{Name: name, Hash: ref.Hash, Files: []artifactFileJSON{}}
			for _, f := range d.Files.ListArtifactFiles(row.RepoName, ref.Hash) {
				art.Files = append(art.Files, artifactFileJSON{
					Name: f.Name,
					Size: f.Size,
					URL:  fmt.Sprintf("/api/deploys/%d/artifacts/%s/%s", row.ID, name, url.PathEscape(f.Name)),
				})
			}
			out.Artifacts = append(out.Artifacts, art)
		}
	}
	return out
}

// previewURL builds http://<short>.<repo>.<domain>[:port]/ from the listen
// address.
func (d Deps) previewURL(row db.DeployRow) string {
	host := fmt.Sprintf("%s.%s.%s", row.ShortSHA, row.RepoName, d.Config.PreviewDomain)
	if _, port, err := net.SplitHostPort(d.Addr); err == nil && port != "" && port != "80" {
		host += ":" + port
	}
	return "http://" + host + "/"
}

func (d Deps) handleCreateDeploy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo    string `json:"repo"`
		Ref     string `json:"ref"`
		Rebuild bool   `json:"rebuild"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Repo == "" || req.Ref == "" {
		httpError(w, http.StatusBadRequest, "repo and ref are required")
		return
	}
	row, err := d.Queue.RequestDeploy(r.Context(), req.Repo, req.Ref, req.Rebuild)
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, fmt.Sprintf("repo %q is not registered", req.Repo))
		return
	}
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, d.deployJSON(row))
}

func (d Deps) handleListDeploys(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := d.Store.ListDeploys(db.DeployFilter{
		Repo:   q.Get("repo"),
		Branch: q.Get("branch"),
		Author: q.Get("author"),
	})
	if err != nil {
		internalError(w, "list deploys", err)
		return
	}
	out := make([]deployJSON, len(rows))
	for i, row := range rows {
		out[i] = d.deployJSON(row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) deployFromPath(w http.ResponseWriter, r *http.Request) (db.DeployRow, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid deploy id")
		return db.DeployRow{}, false
	}
	row, err := d.Store.GetDeployByID(id)
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "deploy not found")
		return db.DeployRow{}, false
	}
	if err != nil {
		internalError(w, "get deploy", err)
		return db.DeployRow{}, false
	}
	return row, true
}

func (d Deps) handleGetDeploy(w http.ResponseWriter, r *http.Request) {
	row, ok := d.deployFromPath(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, d.deployJSON(row))
}

// handleStopDeploy stops the deploy's supervised processes without removing
// it; they cold-start again on the next request. Because processes are shared
// per artifact hash, sibling deploys on the same hash stop too. Returns the
// updated deploy (its process state now reads idle).
func (d Deps) handleStopDeploy(w http.ResponseWriter, r *http.Request) {
	row, ok := d.deployFromPath(w, r)
	if !ok {
		return
	}
	d.Super.StopDeploy(row, "stopped via API")
	writeJSON(w, http.StatusOK, d.deployJSON(row))
}

// handleDeleteDeploy hard-deletes a deploy: it removes the row, then stops and
// garbage-collects any artifacts, backend state, and process bookkeeping no
// surviving deploy still references (see supervise.Manager.GCDeploy). On-disk
// cleanup is best-effort — orphans only cost disk — so it never fails the
// request once the row is gone.
func (d Deps) handleDeleteDeploy(w http.ResponseWriter, r *http.Request) {
	row, ok := d.deployFromPath(w, r)
	if !ok {
		return
	}
	if err := d.Store.DeleteDeploy(row.ID); err != nil {
		internalError(w, "delete deploy", err)
		return
	}
	d.Super.GCDeploy(row)
	w.WriteHeader(http.StatusNoContent)
}

// handleArtifactDownload serves one file of a named downloadable artifact.
func (d Deps) handleArtifactDownload(w http.ResponseWriter, r *http.Request) {
	row, ok := d.deployFromPath(w, r)
	if !ok {
		return
	}
	if row.Status != db.DeployReady {
		httpError(w, http.StatusConflict, fmt.Sprintf("deploy is %s, not ready", row.Status))
		return
	}
	ref, ok := row.Artifacts[r.PathValue("name")]
	if !ok {
		httpError(w, http.StatusNotFound, "no such artifact")
		return
	}
	// Path values are decoded, so a segment can smuggle separators
	// (%2F, %5C); published files are flat base names, never nested.
	file := r.PathValue("file")
	if file == "" || file == "." || file == ".." || strings.ContainsAny(file, `/\`) {
		httpError(w, http.StatusNotFound, "no such file")
		return
	}
	path := filepath.Join(d.Files.ArtifactDir(row.RepoName, ref.Hash), file)
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		httpError(w, http.StatusNotFound, "no such file")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", file))
	http.ServeFile(w, r, path)
}

// handleDeployLogs returns a plain-text snapshot of every build log.
// (?follow=1 streaming arrives in M2.)
func (d Deps) handleDeployLogs(w http.ResponseWriter, r *http.Request) {
	row, ok := d.deployFromPath(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	parts := []struct{ title, path string }{
		{"frontend build", row.FeBuildLogPath},
		{"backend build", row.BeBuildLogPath},
	}
	for _, name := range slices.Sorted(maps.Keys(row.Artifacts)) {
		parts = append(parts, struct{ title, path string }{
			"artifacts." + name + " build", row.Artifacts[name].LogPath,
		})
	}
	for _, part := range parts {
		fmt.Fprintf(w, "--- %s ---\n", part.title)
		if part.path == "" {
			fmt.Fprintln(w, "(not started)")
			continue
		}
		b, err := os.ReadFile(part.path)
		if err != nil {
			fmt.Fprintln(w, "(no log)")
			continue
		}
		w.Write(b)
		fmt.Fprintln(w)
	}
}

// runLogTailBytes caps how much history a fresh run-log view loads; the
// client keeps the returned offset and receives only appended bytes after.
const runLogTailBytes = 256 * 1024

// runLogChunkBytes caps one response; a lagging client catches up across
// polls.
const runLogChunkBytes = 1 << 20

// runLogChunk is one incremental slice of a process run log.
type runLogChunk struct {
	Side    string `json:"side"`
	Attempt int    `json:"attempt"` // Nth start of the artifact; 0 = never started
	Offset  int64  `json:"offset"`  // echo back to receive only new bytes
	Content string `json:"content"`
	// Truncated marks a fresh view that skipped history beyond the tail cap.
	Truncated bool `json:"truncated,omitempty"`
	// Process is the side's live state, so a log view can label itself.
	Process string `json:"process,omitempty"`
}

// sideKey resolves the ?side= query to the deploy's supervisor key and
// artifact hash. ok=false means the error response was already written.
func (d Deps) sideKey(w http.ResponseWriter, r *http.Request, row db.DeployRow) (side, hash string, key supervise.Key, ok bool) {
	side = r.URL.Query().Get("side")
	switch side {
	case "", "be":
		side = "be"
		return side, row.BeHash, supervise.BackendKey(row.RepoID, row.BeHash), true
	case "fe":
		if row.FeHash != "" {
			if _, err := d.Store.GetFrontendArtifact(row.RepoID, row.FeHash); err != nil {
				httpError(w, http.StatusNotFound, "deploy has no frontend process (static frontend)")
				return "", "", key, false
			}
		}
		return side, row.FeHash, supervise.FrontendKey(row.RepoID, row.FeHash, row.BeHash), true
	default:
		httpError(w, http.StatusBadRequest, `side must be "be" or "fe"`)
		return "", "", key, false
	}
}

// handleDeployRunLog returns an incremental slice of a run log — the
// supervised process's combined stdout+stderr (init output included), the
// docker-logs view of a preview. Each call returns the latest start
// attempt's log from the requested offset; echoing attempt and offset back
// yields only new bytes, and a restart (new attempt) resets the view to a
// tail of the new file. Run logs outlive their process, so crash output is
// readable after the exit.
func (d Deps) handleDeployRunLog(w http.ResponseWriter, r *http.Request) {
	row, ok := d.deployFromPath(w, r)
	if !ok {
		return
	}
	side, hash, key, ok := d.sideKey(w, r, row)
	if !ok {
		return
	}
	if hash == "" {
		// No artifact yet (still building) or the deploy has no such side.
		writeJSON(w, http.StatusOK, runLogChunk{Side: side})
		return
	}
	chunk := runLogChunk{Side: side, Process: d.Super.Status(key)}

	// Run logs are per artifact (shared by every deploy with the same
	// hash), one numbered file per start attempt.
	dir := filepath.Join(d.Config.LogsDir(), row.RepoName, "run", side+"-"+hash)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if base, found := strings.CutSuffix(e.Name(), ".log"); found {
			if n, err := strconv.Atoi(base); err == nil && n > chunk.Attempt {
				chunk.Attempt = n
			}
		}
	}
	if chunk.Attempt == 0 {
		writeJSON(w, http.StatusOK, chunk)
		return
	}

	f, err := os.Open(filepath.Join(dir, strconv.Itoa(chunk.Attempt)+".log"))
	if err != nil {
		internalError(w, "open run log", err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		internalError(w, "stat run log", err)
		return
	}
	size := info.Size()

	q := r.URL.Query()
	start := int64(0)
	if att, err := strconv.Atoi(q.Get("attempt")); err == nil && att == chunk.Attempt {
		if off, err := strconv.ParseInt(q.Get("offset"), 10, 64); err == nil && off >= 0 && off <= size {
			start = off
		}
	} else if size > runLogTailBytes {
		start = size - runLogTailBytes
		chunk.Truncated = true
	}

	buf := make([]byte, min(size-start, runLogChunkBytes))
	n, err := f.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		internalError(w, "read run log", err)
		return
	}
	chunk.Content = string(buf[:n])
	chunk.Offset = start + int64(n)
	writeJSON(w, http.StatusOK, chunk)
}

// sideStats is one side's slice of a deploy stats response. Sampled fields
// are absent while the process isn't running (or can't be sampled);
// cpu_percent additionally needs two samples, so it appears from the second
// poll onward.
type sideStats struct {
	State            string   `json:"state"`
	Runtime          string   `json:"runtime,omitempty"`
	CPUPercent       *float64 `json:"cpu_percent,omitempty"`
	MemoryBytes      *uint64  `json:"memory_bytes,omitempty"`
	MemoryLimitBytes uint64   `json:"memory_limit_bytes,omitempty"`
	StartedAt        string   `json:"started_at,omitempty"`
}

// handleDeployStats reports live resource usage — the docker-stats view of
// a preview — for the deploy's supervised processes. A side the deploy
// doesn't have is null.
func (d Deps) handleDeployStats(w http.ResponseWriter, r *http.Request) {
	row, ok := d.deployFromPath(w, r)
	if !ok {
		return
	}
	sample := func(k supervise.Key) *sideStats {
		s := &sideStats{State: d.Super.Status(k)}
		if ps := d.Super.Stats(r.Context(), k); ps != nil {
			s.Runtime = ps.Runtime
			s.CPUPercent = ps.CPUPercent
			s.MemoryBytes = &ps.MemoryBytes
			s.MemoryLimitBytes = ps.MemoryLimitBytes
			if !ps.StartedAt.IsZero() {
				s.StartedAt = ps.StartedAt.UTC().Format(time.RFC3339)
			}
		}
		return s
	}
	resp := struct {
		Backend  *sideStats `json:"backend"`
		Frontend *sideStats `json:"frontend"`
	}{}
	if row.BeHash != "" {
		resp.Backend = sample(supervise.BackendKey(row.RepoID, row.BeHash))
	}
	if row.FeHash != "" {
		if _, err := d.Store.GetFrontendArtifact(row.RepoID, row.FeHash); err == nil {
			resp.Frontend = sample(supervise.FrontendKey(row.RepoID, row.FeHash, row.BeHash))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

// httpError writes a JSON error body so API clients never have to parse
// plain-text errors.
func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalError logs the underlying error and hides it from the client.
func internalError(w http.ResponseWriter, op string, err error) {
	log.Printf("%s: %v", op, err)
	httpError(w, http.StatusInternalServerError, "internal server error")
}
