package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/ingest"
	"github.com/jmelahman/local-preview/internal/s3store"
)

type ingestionJSON struct {
	ID           string               `json:"id"`
	Status       string               `json:"status"`
	Error        string               `json:"error,omitempty"`
	UploadURL    string               `json:"upload_url,omitempty"`
	UploadFields map[string]string    `json:"upload_fields,omitempty"`
	Result       *ingestionResultJSON `json:"result,omitempty"`
}

type ingestionResultJSON struct {
	Upload *build.UploadResult `json:"upload,omitempty"`
	Deploy *deployJSON         `json:"deploy,omitempty"`
}

func ingestionRoute(method, path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/repos/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] != "ingestions" {
		return false
	}
	if len(parts) == 2 {
		return method == http.MethodPost
	}
	if parts[2] == "" {
		return false
	}
	if len(parts) == 3 {
		return method == http.MethodGet || method == http.MethodDelete
	}
	return len(parts) == 4 && parts[3] == "submit" && method == http.MethodPost
}

func (d Deps) authorizeIngestion(w http.ResponseWriter, r *http.Request) (string, bool) {
	return d.authorizeIngestionRepo(w, r, r.PathValue("repo"))
}

func (d Deps) authorizeIngestionRepo(w http.ResponseWriter, r *http.Request, repoName string) (string, bool) {
	if claims, ok := oidcClaimsFrom(r.Context()); ok {
		return claims.Actor, d.oidcMayActOn(w, claims, repoName, "manage ingestions for")
	}
	if authenticated, _ := r.Context().Value(ssoRequestKey{}).(bool); authenticated {
		if sess, ok := d.apexSession(r); ok {
			return sess.GitHubLogin, true
		}
		return "", true
	}
	if d.UploadAuth != nil && !d.authorizeUpload(w, r, repoName) {
		return "", false
	}
	return "", true
}

func (d Deps) handleReserveIngestion(w http.ResponseWriter, r *http.Request) {
	actor, ok := d.authorizeIngestion(w, r)
	if !ok {
		return
	}
	if d.Ingestion == nil {
		httpError(w, http.StatusServiceUnavailable, "ingestion is not configured")
		return
	}
	var req ingest.Request
	if !decodeJSON(w, r, &req) {
		return
	}
	job, signed, err := d.Ingestion.Reserve(r.Context(), r.PathValue("repo"), req, actor)
	if err != nil {
		ingestionError(w, "reserve ingestion", err)
		return
	}
	d.writeIngestion(w, r, http.StatusAccepted, job, signed)
}

func (d Deps) ingestionJob(w http.ResponseWriter, r *http.Request) (db.Job, bool) {
	if _, ok := d.authorizeIngestion(w, r); !ok {
		return db.Job{}, false
	}
	if d.Ingestion == nil {
		httpError(w, http.StatusServiceUnavailable, "ingestion is not configured")
		return db.Job{}, false
	}
	repo, err := d.Store.GetRepoByName(r.PathValue("repo"))
	if err != nil {
		ingestionError(w, "get ingestion repo", err)
		return db.Job{}, false
	}
	job, err := d.Ingestion.Get(r.PathValue("id"))
	if err != nil {
		ingestionError(w, "get ingestion", err)
		return db.Job{}, false
	}
	if job.RepoID != repo.ID {
		httpError(w, http.StatusNotFound, "ingestion not found")
		return db.Job{}, false
	}
	return job, true
}

func (d Deps) handleSubmitIngestion(w http.ResponseWriter, r *http.Request) {
	job, ok := d.ingestionJob(w, r)
	if !ok {
		return
	}
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	job, err := d.Ingestion.Submit(r.Context(), job.ID)
	if err != nil {
		ingestionError(w, "submit ingestion", err)
		return
	}
	d.writeIngestion(w, r, http.StatusAccepted, job, s3store.StagedUpload{})
}

func (d Deps) handleGetIngestion(w http.ResponseWriter, r *http.Request) {
	job, ok := d.ingestionJob(w, r)
	if !ok {
		return
	}
	d.writeIngestion(w, r, http.StatusOK, job, s3store.StagedUpload{})
}

func (d Deps) handleCancelIngestion(w http.ResponseWriter, r *http.Request) {
	job, ok := d.ingestionJob(w, r)
	if !ok {
		return
	}
	if err := d.Ingestion.Cancel(job.ID); err != nil {
		ingestionError(w, "cancel ingestion", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) writeIngestion(w http.ResponseWriter, r *http.Request, status int, job db.Job, signed s3store.StagedUpload) {
	out := ingestionJSON{ID: job.ID, Status: job.Status, Error: job.Error, UploadURL: signed.URL, UploadFields: signed.Fields}
	if job.Status == db.JobDone {
		result, err := d.Ingestion.Result(r.Context(), job)
		if err != nil {
			ingestionError(w, "read ingestion result", err)
			return
		}
		out.Result = &ingestionResultJSON{Upload: result.Upload}
		if result.Deploy != nil {
			deploy := deployJSON{DeployRow: *result.Deploy}
			if result.Deploy.Status == db.DeployReady {
				deploy.PreviewURL = d.previewURL(*result.Deploy)
			}
			out.Result.Deploy = &deploy
		}
	}
	writeJSON(w, status, out)
}

func ingestionError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		httpError(w, http.StatusNotFound, "ingestion or repository not found")
	case errors.Is(err, db.ErrJobFull):
		w.Header().Set("Retry-After", "5")
		httpError(w, http.StatusTooManyRequests, "ingestion queue is full")
	case errors.Is(err, db.ErrJobConflict), errors.Is(err, db.ErrJobNotReady), errors.Is(err, db.ErrConflict):
		httpError(w, http.StatusConflict, "ingestion state conflicts with this request")
	case errors.Is(err, ingest.ErrValidation):
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		internalError(w, operation, err)
	}
}
