package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func TestUploadCommandStagedJSONSpoolsDirectory(t *testing.T) {
	isolateConfig(t)
	t.Setenv("PREVIEW_UPLOAD_TOKEN", "test-token")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bundle.js"), []byte("console.log('preview')"), 0600); err != nil {
		t.Fatal(err)
	}
	var wantSize int64
	var wantHash string
	uploaded := false
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("token leaked to storage")
		}
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
			return
		}
		defer func() {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				t.Error(err)
			}
		}()
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		archive, err := io.ReadAll(f)
		if err != nil {
			t.Error(err)
			return
		}
		digest := sha256.Sum256(archive)
		if int64(len(archive)) != wantSize || hex.EncodeToString(digest[:]) != wantHash {
			t.Error("archive metadata mismatch")
		}
		gz, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			t.Error(err)
			return
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		hdr, err := tr.Next()
		if err != nil {
			t.Error(err)
			return
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Error(err)
			return
		}
		if hdr.Name != "bundle.js" || string(data) != "console.log('preview')" {
			t.Errorf("unexpected archive %s: %s", hdr.Name, data)
		}
		uploaded = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer storage.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing control token")
		}
		if strings.HasSuffix(r.URL.Path, "/submit") {
			if !uploaded {
				t.Error("submitted before transfer")
			}
			if _, err := io.WriteString(w, `{"id":"job","status":"done","result":{"upload":{"sha":"abcdef","short_sha":"abcdef","side":"frontend","hash":"content-hash","published":true},"deploy":{"id":9,"status":"ready","preview_url":"https://abcdef.preview.test"}}}`); err != nil {
				t.Error(err)
			}
			return
		}
		var req struct {
			ArchiveSize   int64  `json:"archive_size"`
			ArchiveSHA256 string `json:"archive_sha256"`
			Deploy        bool   `json:"deploy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if !req.Deploy {
			t.Error("deploy flag missing")
		}
		wantSize, wantHash = req.ArchiveSize, req.ArchiveSHA256
		if err := json.NewEncoder(w).Encode(map[string]any{"id": "job", "status": "uploading", "upload_url": storage.URL, "upload_fields": map[string]string{"key": "staged/job"}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	cmd := uploadCmd()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"frontend", dir, "abcdef", "--repo", "demo", "--server", server.URL, "--staged", "--deploy", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	decoder := json.NewDecoder(&out)
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if _, ok := result["upload"]; !ok {
		t.Error("upload result missing")
	}
	if _, ok := result["deploy"]; !ok {
		t.Error("deploy result missing")
	}
	if decoder.Decode(&result) != io.EOF {
		t.Error("stdout contained more than one JSON result")
	}
}

func TestUploadCommandKeepsSynchronousDefault(t *testing.T) {
	isolateConfig(t)
	t.Setenv("PREVIEW_UPLOAD_TOKEN", "")
	archive := filepath.Join(t.TempDir(), "bundle.tar")
	if err := os.WriteFile(archive, []byte("existing archive"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/repos/demo/uploads/frontend" || r.URL.Query().Get("ref") != "abcdef" {
			t.Errorf("unexpected route %s", r.URL)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "existing archive" {
			t.Error("archive changed")
		}
		if _, err := io.WriteString(w, `{"sha":"abcdef","short_sha":"abcdef","side":"frontend","hash":"content-hash","published":true}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	cmd := uploadCmd()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"frontend", archive, "abcdef", "--repo", "demo", "--server", server.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "uploaded frontend for demo@abcdef") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestUploadCommandJSONRequiresStaged(t *testing.T) {
	isolateConfig(t)
	cmd := uploadCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"frontend", "unused", "abcdef", "--repo", "demo", "--json"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--json requires --staged") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestDeployCommandOIDCRefreshesTokensForAsyncBuild(t *testing.T) {
	isolateConfig(t)
	t.Setenv("PREVIEW_UPLOAD_TOKEN", "")
	var minted atomic.Int64
	audience := ""
	oidc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer runner-token" || r.URL.Query().Get("audience") != audience {
			t.Error("invalid runner token request")
		}
		if err := json.NewEncoder(w).Encode(map[string]string{"value": fmt.Sprintf("token-%d", minted.Add(1))}); err != nil {
			t.Error(err)
		}
	}))
	defer oidc.Close()
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", minted.Load()) {
			t.Error("stale OIDC token")
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/deploys" {
			if _, err := io.WriteString(w, `{"id":"job","status":"queued","repo":"demo"}`); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path != "/api/repos/demo/ingestions/job" {
			t.Errorf("unexpected path%s", r.URL.Path)
		}
		polls++
		if polls == 1 {
			if _, err := io.WriteString(w, `{"id":"job","status":"leased"}`); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := io.WriteString(w, `{"id":"job","status":"done","result":{"deploy":{"id":9,"status":"ready","preview_url":"https://preview.test"}}}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	audience = server.URL
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", oidc.URL)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runner-token")
	t.Setenv("PREVIEW_GITHUB_OIDC_AUDIENCE", server.URL)
	cmd := deployCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"main", "--repo", "demo", "--server", server.URL, "--oidc", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != 9 || result.Status != "ready" || minted.Load() != 3 || polls != 2 {
		t.Fatalf("result%+v tokens%d polls%d", result, minted.Load(), polls)
	}
}
