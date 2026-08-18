package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
	if d.UploadAuth != nil && !d.authorizeUpload(w, r, repo) {
		return
	}
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

// authorizeUpload gates an upload behind a GitHub Actions OIDC token when
// UploadAuth is configured. It returns true when the request may proceed; on
// any failure it writes the error response and returns false.
//
// Order matters: the token is verified before the repo is looked up, so an
// unauthenticated caller can't probe which repos are registered. A verified
// token then authorizes only the repo whose registered source is the same
// GitHub repository the token was minted for — repo A's workflow can never
// upload to repo B.
func (d Deps) authorizeUpload(w http.ResponseWriter, r *http.Request, repoName string) bool {
	token, ok := bearerToken(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		httpError(w, http.StatusUnauthorized, "missing bearer token; upload requires a GitHub Actions OIDC token")
		return false
	}
	claims, err := d.UploadAuth.Verify(r.Context(), token)
	if err != nil {
		httpError(w, http.StatusForbidden, "invalid OIDC token")
		return false
	}
	repo, err := d.Store.GetRepoByName(repoName)
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, fmt.Sprintf("repo %q is not registered", repoName))
		return false
	}
	if err != nil {
		internalError(w, "get repo", err)
		return false
	}
	if !sourceMatchesGitHubRepo(repo.Source, claims.Repository) {
		httpError(w, http.StatusForbidden, fmt.Sprintf(
			"token for %q is not authorized to upload to repo %q", claims.Repository, repoName))
		return false
	}
	return true
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. The scheme name is case-insensitive per RFC 6750.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < len("Bearer ") || !strings.EqualFold(h[:7], "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(h[7:])
	return token, token != ""
}

// sourceMatchesGitHubRepo reports whether a registered repo's source names the
// same GitHub repository as an OIDC token's "repository" claim (owner/name).
// Both are reduced with normalizeGitURL, so the source may be any clone-URL
// form (https, ssh, scp-like, with or without .git) and case doesn't matter.
func sourceMatchesGitHubRepo(source, repository string) bool {
	if repository == "" {
		return false
	}
	return normalizeGitURL(source) == normalizeGitURL("https://github.com/"+repository)
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
