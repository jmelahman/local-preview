package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/db"
)

// handleUploadFrontend, handleUploadBackend, and handleUploadArtifact publish a
// CI-built side into the content-addressed store for a commit, so a deploy of
// that commit (or any commit sharing the hash) serves it without building. See
// build.Queue.Upload — the server resolves ref → sha, reads the manifest, and
// computes the same content-address a build would target.
func (d Deps) handleUploadFrontend(w http.ResponseWriter, r *http.Request) {
	d.handleUpload(w, r, build.SideFrontend, "")
}

func (d Deps) handleUploadBackend(w http.ResponseWriter, r *http.Request) {
	d.handleUpload(w, r, build.SideBackend, "")
}

func (d Deps) handleUploadArtifact(w http.ResponseWriter, r *http.Request) {
	d.handleUpload(w, r, build.SideArtifact, r.PathValue("name"))
}

func (d Deps) handleUpload(w http.ResponseWriter, r *http.Request, side, name string) {
	repo := r.PathValue("repo")
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		httpError(w, http.StatusBadRequest, "ref query parameter is required")
		return
	}
	overwrite, err := boolQuery(r, "overwrite")
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := d.Queue.Upload(r.Context(), repo, ref, side, name, r.Body, overwrite)
	switch {
	case errors.Is(err, db.ErrNotFound):
		httpError(w, http.StatusNotFound, fmt.Sprintf("repo %q is not registered", repo))
	case errors.Is(err, build.ErrNoSuchArtifact):
		httpError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, build.ErrRepoNotReady):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		// Unresolvable ref, manifest parse error, malformed tar, or a declared
		// artifact file missing from the upload — all caller-fixable input.
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// boolQuery parses an optional boolean query parameter; absent means false.
func boolQuery(r *http.Request, key string) (bool, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return v, nil
}
