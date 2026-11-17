package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func archiveFile(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStagedUploadStreamsArchiveAndRefreshesTokens(t *testing.T) {
	data := bytes.Repeat([]byte("archive bytes\x00"), 8192)
	archive := archiveFile(t, data)
	digest := sha256.Sum256(data)
	var uploaded atomic.Bool
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("control token leaked to storage")
		}
		if r.ContentLength <= int64(len(data)) {
			t.Error("multipart Content-Length missing")
		}
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer func() {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				t.Error(err)
			}
		}()
		if r.FormValue("policy") != "signed-policy" || r.FormValue("key") != "staged/job" {
			t.Error("missing signed fields")
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		got, err := io.ReadAll(f)
		if err != nil {
			t.Error(err)
		}
		if !bytes.Equal(got, data) {
			t.Error("archive changed")
		}
		uploaded.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer storage.Close()
	tokens := 0
	reserveAttempts := 0
	polls := 0
	requestKey := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), fmt.Sprintf("Bearer token-%d", tokens); got != want {
			t.Errorf("auth=%q want%q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/repos/demo/ingestions":
			var req struct {
				RequestKey    string `json:"request_key"`
				ArchiveSize   int64  `json:"archive_size"`
				ArchiveSHA256 string `json:"archive_sha256"`
				Deploy        bool   `json:"deploy"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if len(req.RequestKey) != 36 {
				t.Errorf("not UUID: %q", req.RequestKey)
			}
			if requestKey != "" && requestKey != req.RequestKey {
				t.Error("retry changed idempotency key")
			}
			requestKey = req.RequestKey
			if req.ArchiveSize != int64(len(data)) || req.ArchiveSHA256 != hex.EncodeToString(digest[:]) || !req.Deploy {
				t.Errorf("bad archive metadata %+v", req)
			}
			reserveAttempts++
			if reserveAttempts == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusServiceUnavailable)
				if _, err := io.WriteString(w, `{"error":"busy"}`); err != nil {
					t.Error(err)
				}
				return
			}
			if err := json.NewEncoder(w).Encode(ingestionResponse{ID: "job", Status: "uploading", UploadURL: storage.URL, UploadFields: map[string]string{"key": "staged/job", "policy": "signed-policy"}}); err != nil {
				t.Error(err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/submit"):
			if !uploaded.Load() {
				t.Error("submitted before upload")
			}
			if _, err := io.WriteString(w, `{"id":"job","status":"queued"}`); err != nil {
				t.Error(err)
			}
		case r.Method == http.MethodGet:
			polls++
			if polls == 1 {
				if _, err := io.WriteString(w, `{"id":"job","status":"leased"}`); err != nil {
					t.Error(err)
				}
				return
			}
			if polls == 2 {
				if _, err := io.WriteString(w, `{"id":"job","status":"publishing"}`); err != nil {
					t.Error(err)
				}
				return
			}
			if _, err := io.WriteString(w, `{"id":"job","status":"done","result":{"upload":{"sha":"abcdef","short_sha":"abcdef","side":"frontend","hash":"server-hash","published":true},"deploy":{"id":7,"status":"ready","preview_url":"https://preview.example"}}}`); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := New(server.URL, nil).SetTokenProvider(func(context.Context) (string, error) { tokens++; return fmt.Sprintf("token-%d", tokens), nil })
	got, err := c.StagedUpload(context.Background(), "demo", "frontend", "", "abcdef", false, true, archive)
	if err != nil {
		t.Fatal(err)
	}
	if got.Upload.Hash != "server-hash" || got.Deploy == nil || got.Deploy.ID != 7 {
		t.Fatalf("unexpected result %+v", got)
	}
	if tokens != 6 || polls != 3 || reserveAttempts != 2 {
		t.Fatalf("tokens%d polls%d reservations%d", tokens, polls, reserveAttempts)
	}
}

func TestStagedUploadCancellationDeletesJobWithFreshContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var deleted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if _, err := io.WriteString(w, `{"id":"job","status":"queued"}`); err != nil {
				t.Error(err)
			}
		case http.MethodDelete:
			deleted.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s", r.Method)
		}
	}))
	defer server.Close()
	requests := 0
	c := New(server.URL, nil).SetTokenProvider(func(context.Context) (string, error) {
		requests++
		if requests == 2 {
			cancel()
			return "", ctx.Err()
		}
		return "", nil
	})
	_, err := c.StagedUpload(ctx, "demo", "frontend", "", "abc", false, false, archiveFile(t, []byte("tar")))
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if !deleted.Load() {
		t.Fatal("accepted job was not cancelled")
	}
}

func TestStagedUploadReportsJobFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, `{"id":"job","status":"failed","error":"archive digest mismatch"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	_, err := New(server.URL, nil).StagedUpload(context.Background(), "demo", "frontend", "", "abc", false, false, archiveFile(t, []byte("tar")))
	if err == nil || !strings.Contains(err.Error(), "archive digest mismatch") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestStagedUploadRejectsStorage4xxWithoutRetry(t *testing.T) {
	attempts := 0
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts++; w.WriteHeader(http.StatusForbidden) }))
	defer storage.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(ingestionResponse{ID: "job", Status: "uploading", UploadURL: storage.URL}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	_, err := New(server.URL, nil).StagedUpload(context.Background(), "demo", "frontend", "", "abc", false, false, archiveFile(t, []byte("tar")))
	if err == nil || !strings.Contains(err.Error(), "403") || attempts != 1 {
		t.Fatalf("attempts%d error%v", attempts, err)
	}
}

func TestSetTokenOverridesProvider(t *testing.T) {
	c := New("http://unused", nil).SetTokenProvider(func(context.Context) (string, error) { t.Fatal("provider should not run"); return "", nil }).SetToken("static")
	token, err := c.requestToken(context.Background())
	if err != nil || token != "static" {
		t.Fatalf("token%q error%v", token, err)
	}
}

func TestCreateDeployWaitsForIngestionWithFreshTokens(t *testing.T) {
	tokenCount := 0
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", tokenCount) {
			t.Error("stale auth token")
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/deploys" {
			if _, err := io.WriteString(w, `{"id":"build-job","status":"queued","repo":"demo"}`); err != nil {
				t.Error(err)
			}
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/repos/demo/ingestions/build-job" {
			t.Errorf("unexpected route%s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		polls++
		if polls == 1 {
			if _, err := io.WriteString(w, `{"id":"build-job","status":"leased"}`); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := io.WriteString(w, `{"id":"build-job","status":"done","result":{"deploy":{"id":11,"repo":"demo","status":"ready","preview_url":"https://preview.example"}}}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := New(server.URL, nil).SetTokenProvider(func(context.Context) (string, error) { tokenCount++; return fmt.Sprintf("token-%d", tokenCount), nil })
	result, err := c.CreateDeploy(context.Background(), "demo", "main", false)
	if err != nil || result.ID != 11 || result.Status != "ready" || polls != 2 || tokenCount != 3 {
		t.Fatalf("result%+v error%v polls%d tokens%d", result, err, polls, tokenCount)
	}
}

func TestCreateDeployKeepsLegacyNumericResponse(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if _, err := io.WriteString(w, `{"id":12,"repo":"demo","status":"queued"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	result, err := New(server.URL, nil).CreateDeploy(context.Background(), "demo", "main", false)
	if err != nil || result.ID != 12 || result.Status != "queued" || calls != 1 {
		t.Fatalf("result%+v error%v calls%d", result, err, calls)
	}
}

func TestCreateDeploySurfacesIngestionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, `{"id":"job","status":"failed","error":"worker build failed"}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	_, err := New(server.URL, nil).CreateDeploy(context.Background(), "demo", "main", false)
	if err == nil || !strings.Contains(err.Error(), "worker build failed") {
		t.Fatalf("unexpected error%v", err)
	}
}

func TestCreateDeployReadsResultOfAlreadyCompletedJob(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == http.MethodPost {
			if _, err := io.WriteString(w, `{"id":"cached","status":"done","repo":"demo"}`); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path != "/api/repos/demo/ingestions/cached" {
			t.Errorf("unexpected result path%s", r.URL.Path)
		}
		if _, err := io.WriteString(w, `{"id":"cached","status":"done","result":{"deploy":{"id":15,"status":"ready","preview_url":"https://preview.example"}}}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	result, err := New(server.URL, nil).CreateDeploy(context.Background(), "demo", "abc", false)
	if err != nil || result.ID != 15 || calls != 2 {
		t.Fatalf("result%+v error%v calls%d", result, err, calls)
	}
}
