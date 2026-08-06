// Package api defines the HTTP surface served at the apex host: JSON
// endpoints under /api/ plus the embedded dashboard SPA at /.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/config"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/supervise"
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
	mux.HandleFunc("DELETE /api/repos/{name}", d.handleDeleteRepo)
	mux.HandleFunc("POST /api/deploys", d.handleCreateDeploy)
	mux.HandleFunc("GET /api/deploys", d.handleListDeploys)
	mux.HandleFunc("GET /api/deploys/{id}", d.handleGetDeploy)
	mux.HandleFunc("GET /api/deploys/{id}/logs", d.handleDeployLogs)
	mux.Handle("/", web.Handler())
	return mux
}

func (d Deps) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": d.Build.Version,
	})
}

func (d Deps) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Source string `json:"source"`
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
	writeJSON(w, http.StatusCreated, repo)
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
// process-mode frontends.
type deployJSON struct {
	db.DeployRow
	PreviewURL string `json:"preview_url,omitempty"`
	Process    string `json:"process,omitempty"`
	FeProcess  string `json:"fe_process,omitempty"`
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

// handleDeployLogs returns a plain-text snapshot of both build logs.
// (?follow=1 streaming arrives in M2.)
func (d Deps) handleDeployLogs(w http.ResponseWriter, r *http.Request) {
	row, ok := d.deployFromPath(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, part := range []struct{ title, path string }{
		{"frontend build", row.FeBuildLogPath},
		{"backend build", row.BeBuildLogPath},
	} {
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
