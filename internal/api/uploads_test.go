package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func doUpload(t *testing.T, mux *http.ServeMux, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

type uploadResp struct {
	SHA       string `json:"sha"`
	Side      string `json:"side"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
	Published bool   `json:"published"`
	Files     []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

func TestUploadEndpoints(t *testing.T) {
	mux, _ := newTestMux(t)
	src := newSourceRepo(t)
	manifest := fixtureManifest + `
[artifacts.cli]
path  = "srv"
build = [["sh", "-c", "echo built > mycli"]]
files = ["mycli"]
`
	if err := os.WriteFile(filepath.Join(src, "preview.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, src, "commit", "-qam", "declare artifact")
	registerRepo(t, mux, "demo", src)

	// Frontend upload primes the content store.
	fe := tarGz(t, map[string]string{"index.html": "UPLOADED"})
	rec := doUpload(t, mux, "/api/repos/demo/uploads/frontend?ref=main", fe)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload frontend: %d %s", rec.Code, rec.Body)
	}
	var res uploadResp
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Published || res.Side != "frontend" || res.Hash == "" {
		t.Fatalf("frontend upload response = %+v", res)
	}
	feHash := res.Hash

	// A second identical upload is a no-op.
	rec = doUpload(t, mux, "/api/repos/demo/uploads/frontend?ref=main", fe)
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Published {
		t.Fatalf("second upload should not have republished: %+v", res)
	}

	// Artifact upload.
	rec = doUpload(t, mux, "/api/repos/demo/uploads/artifacts/cli?ref=main", tarGz(t, map[string]string{"mycli": "UPLOADED-CLI"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload artifact: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Published || len(res.Files) != 1 || res.Files[0].Name != "mycli" {
		t.Fatalf("artifact upload response = %+v", res)
	}
	artHash := res.Hash

	// Deploying the commit reuses both uploads: fe_hash matches, and the
	// artifact is ready at the uploaded hash without an artifact build phase.
	rec = doJSON(t, mux, "POST", "/api/deploys", `{"repo":"demo","ref":"main"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create deploy: %d %s", rec.Code, rec.Body)
	}
	var d struct {
		ID        int64  `json:"id"`
		Status    string `json:"status"`
		FeHash    string `json:"fe_hash"`
		Artifacts []struct {
			Name   string `json:"name"`
			Hash   string `json:"hash"`
			Status string `json:"status"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for d.Status != "ready" {
		if d.Status == "failed" || time.Now().After(deadline) {
			logs := doJSON(t, mux, "GET", "/api/deploys/1/logs", "")
			t.Fatalf("deploy status = %s; logs:\n%s", d.Status, logs.Body)
		}
		time.Sleep(50 * time.Millisecond)
		rec = doJSON(t, mux, "GET", "/api/deploys/1", "")
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
	}
	if d.FeHash != feHash {
		t.Fatalf("deploy fe_hash = %s, want the uploaded %s", d.FeHash, feHash)
	}
	if len(d.Artifacts) != 1 || d.Artifacts[0].Hash != artHash || d.Artifacts[0].Status != "ready" {
		t.Fatalf("deploy artifacts = %+v, want cli ready at %s", d.Artifacts, artHash)
	}

	// Error mapping.
	for _, tc := range []struct {
		name string
		path string
		code int
	}{
		{"unknown repo", "/api/repos/nope/uploads/frontend?ref=main", http.StatusNotFound},
		{"unknown artifact", "/api/repos/demo/uploads/artifacts/ghost?ref=main", http.StatusNotFound},
		{"missing ref", "/api/repos/demo/uploads/frontend", http.StatusBadRequest},
		{"bad ref", "/api/repos/demo/uploads/frontend?ref=no-such-branch", http.StatusBadRequest},
		{"bad overwrite", "/api/repos/demo/uploads/frontend?ref=main&overwrite=maybe", http.StatusBadRequest},
	} {
		if rec := doUpload(t, mux, tc.path, fe); rec.Code != tc.code {
			t.Fatalf("%s: got %d, want %d (%s)", tc.name, rec.Code, tc.code, rec.Body)
		}
	}
}
