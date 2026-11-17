package ingest_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/api"
	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/client"
	"github.com/jmelahman/local-preview/internal/config"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/gittest"
	"github.com/jmelahman/local-preview/internal/ingest"
	"github.com/jmelahman/local-preview/internal/s3store"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

func TestMain(m *testing.M) {
	gittest.IsolateEnv()
	os.Exit(m.Run())
}

func localTestTier(t *testing.T) *s3store.Tier {
	t.Helper()
	endpoint, bucket := os.Getenv("PREVIEW_TEST_S3_ENDPOINT"), os.Getenv("PREVIEW_TEST_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set PREVIEW_TEST_S3_ENDPOINT and PREVIEW_TEST_S3_BUCKET for native S3 ingestion tests")
	}
	tier, err := s3store.New(s3store.Config{Endpoint: endpoint, Bucket: bucket, Prefix: "ingest-test-" + rand.Text(), AccessKey: os.Getenv("PREVIEW_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("PREVIEW_TEST_S3_SECRET_KEY"), UseSSL: os.Getenv("PREVIEW_TEST_S3_USE_SSL") == "true", TmpDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return tier
}

type controlFixture struct {
	coord                         *ingest.Coordinator
	database                      *db.Store
	tier                          *s3store.Tier
	files                         *store.Store
	source, root, dbPath, mirror  string
	repo                          db.Repo
	public, private               *httptest.Server
	cancel                        context.CancelFunc
	ctx                           context.Context
	wg                            sync.WaitGroup
	super                         *supervise.Manager
	publicHandler, privateHandler *switchableHandler
}

const workerSecret = "local-integration-worker-secret"

func newControl(t *testing.T, maxPending int) *controlFixture {
	t.Helper()
	tier := localTestTier(t)
	root := t.TempDir()
	databasePath := filepath.Join(root, "control.sqlite")
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	files := store.New(filepath.Join(root, "artifacts"), filepath.Join(root, "state"), filepath.Join(root, "tmp"))
	files.SetArtifactTier(tier)
	git := gitrepo.NewManager(filepath.Join(root, "repos"))
	sup := supervise.New(database, files, filepath.Join(root, "logs"))
	q := build.NewQueue(database, git, files, sup, filepath.Join(root, "logs"), build.HostRunner{})
	q.SetAutoStart(false)
	q.SetArtifactTier(tier)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	c := &controlFixture{database: database, tier: tier, files: files, root: root, dbPath: databasePath, source: t.TempDir(), ctx: ctx, cancel: cancel, super: sup}
	c.coord = &ingest.Coordinator{Store: database, Tier: tier, Queue: q, MaxPending: maxPending, MaxUploadBytes: 64 << 20}
	deps := api.Deps{Store: database, Git: git, Files: files, Super: sup, Queue: q, Ingestion: c.coord, Config: config.Config{Preview: config.PreviewBase{Domain: "preview.test", Scheme: "http"}}}
	c.publicHandler = &switchableHandler{handler: api.AuthMiddleware(deps, api.NewMux(deps))}
	c.privateHandler = &switchableHandler{handler: c.coord.Handler(workerSecret)}
	c.public = httptest.NewServer(c.publicHandler)
	c.private = httptest.NewServer(c.privateHandler)
	t.Cleanup(func() {
		cancel()
		c.wg.Wait()
		c.public.Close()
		c.private.Close()
		c.super.StopAll()
		c.database.Close()
	})
	return c
}

func (c *controlFixture) sourceRepo(t *testing.T, buildScript string) string {
	t.Helper()
	for _, dir := range []string{"web", "backend"} {
		if err := os.MkdirAll(filepath.Join(c.source, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	commands, _ := json.Marshal([][]string{{"sh", "-c", buildScript}})
	manifest := fmt.Sprintf("[frontend]\npath = \"web\"\ndist = \"dist\"\nbuild = [[\"sh\",\"-c\",\"exit 91\"]]\n[backend]\npath = \"backend\"\nbuild = %s\nrun = [\"sh\",\"-c\",\"sleep 60\"]\nhealth_path = \"/api/health\"\n", commands)
	for path, data := range map[string]string{"preview.toml": manifest, "web/source.txt": "frontend source", "backend/source.txt": "backend source"} {
		if err := os.WriteFile(filepath.Join(c.source, path), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	gittest.Run(t, c.source, "init", "-q", "-b", "main")
	gittest.Run(t, c.source, "add", "-A")
	gittest.Run(t, c.source, "commit", "-qm", "initial")
	git := gitrepo.NewManager(filepath.Join(c.root, "repos"))
	mirror, err := git.Add(c.ctx, "demo", c.source, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.repo, err = c.database.CreateRepo("demo", c.source, mirror.Path, db.RepoReady)
	if err != nil {
		t.Fatal(err)
	}
	c.mirror = mirror.Path
	if err := os.RemoveAll(c.mirror); err != nil {
		t.Fatal(err)
	}
	return gittest.Run(t, c.source, "rev-parse", "HEAD")
}

func archive(t *testing.T, content string) (string, []byte) {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len([]byte(content)))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "input.tar.gz")
	if err := os.WriteFile(file, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return file, out.Bytes()
}

func (c *controlFixture) startPublisher() func() {
	ctx, cancel := context.WithCancel(c.ctx)
	done := make(chan struct{})
	coordinator := c.coord
	c.wg.Go(func() { ; defer close(done); coordinator.Run(ctx) })
	return func() { cancel(); <-done }
}

type switchableHandler struct {
	mu      sync.RWMutex
	handler http.Handler
}

func (h *switchableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	h.handler.ServeHTTP(w, r)
}

func (c *controlFixture) restartControl(t *testing.T) {
	t.Helper()
	c.publicHandler.mu.Lock()
	defer c.publicHandler.mu.Unlock()
	c.privateHandler.mu.Lock()
	defer c.privateHandler.mu.Unlock()
	c.super.StopAll()
	if err := c.database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(c.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	c.database = database
	git := gitrepo.NewManager(filepath.Join(c.root, "repos"))
	c.super = supervise.New(database, c.files, filepath.Join(c.root, "logs"))
	queue := build.NewQueue(database, git, c.files, c.super, filepath.Join(c.root, "logs"), build.HostRunner{})
	queue.SetAutoStart(false)
	queue.SetArtifactTier(c.tier)
	old := c.coord
	c.coord = &ingest.Coordinator{Store: database, Tier: c.tier, Queue: queue, MaxPending: old.MaxPending, MaxUploadBytes: old.MaxUploadBytes}
	deps := api.Deps{Store: database, Git: git, Files: c.files, Super: c.super, Queue: queue, Ingestion: c.coord, Config: config.Config{Preview: config.PreviewBase{Domain: "preview.test", Scheme: "http"}}}
	c.publicHandler.handler = api.AuthMiddleware(deps, api.NewMux(deps))
	c.privateHandler.handler = c.coord.Handler(workerSecret)
}
func (c *controlFixture) startWorker(t *testing.T, id string) {
	t.Helper()
	dir := t.TempDir()
	worker := ingest.Worker{ServerURL: c.private.URL, Secret: workerSecret, ID: id, DataDir: dir, Tier: c.tier, Executor: build.Executor{DataDir: dir, Runner: build.HostRunner{}, MaxExtractBytes: 64 << 20}}
	c.wg.Go(func() {
		if err := worker.Run(c.ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("worker: %v", err)
		}
	})
}

func waitFor(t *testing.T, ctx context.Context, check func() bool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func requestJSON(t *testing.T, ctx context.Context, server, secret, method, path string, value, result any) int {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, method, server+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			t.Fatalf("HTTP%d decode: %v", resp.StatusCode, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode
}

func healthy(t *testing.T, ctx context.Context, c *controlFixture) {
	t.Helper()
	check, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if code := requestJSON(t, check, c.public.URL, "", "GET", "/api/health", nil, nil); code != 200 {
		t.Fatalf("health HTTP%d", code)
	}
}

func TestNativeS3ConcurrentClientsKeepControlResponsive(t *testing.T) {
	c := newControl(t, 8)
	defer c.cancel()
	marker := filepath.Join(t.TempDir(), "build-started")
	sha := c.sourceRepo(t, fmt.Sprintf("touch '%s'; sleep 2; printf 'backend-output' > built.txt", marker))
	shas := []string{sha}
	for i := 1; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(c.source, "web", "source.txt"), []byte(fmt.Sprintf("revision-%d", i)), 0644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, c.source, "commit", "-qam", "next frontend")
		shas = append(shas, gittest.Run(t, c.source, "rev-parse", "HEAD"))
	}
	c.startPublisher()
	c.startWorker(t, "worker-one")
	c.startWorker(t, "worker-two")
	type outcome struct {
		result  client.StagedUploadResult
		err     error
		content string
	}
	completed := make(chan outcome, 3)
	for i, sha := range shas {
		content := fmt.Sprintf("<h1>préview 日本語 %d</h1>", i)
		path, _ := archive(t, content)
		go func() {
			result, err := client.New(c.public.URL, nil).StagedUpload(c.ctx, "demo", "frontend", "", sha, false, true, path)
			completed <- outcome{result, err, content}
		}()
	}
	waitFor(t, c.ctx, func() bool {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
		select {
		case out := <-completed:
			t.Fatalf("client completed before backend build started: %v", out.err)
		default:
		}
		return false
	})
	for range 5 {
		healthy(t, c.ctx, c)
	}
	hashes := map[string]bool{}
	for range 3 {
		var out outcome
		select {
		case out = <-completed:
		case <-c.ctx.Done():
			t.Fatal(c.ctx.Err())
		}
		if out.err != nil {
			t.Fatal(out.err)
		}
		if out.result.Deploy == nil || out.result.Deploy.Status != "ready" || out.result.Deploy.PreviewURL == "" || !out.result.Upload.Published {
			t.Fatalf("incomplete result %+v", out.result)
		}
		if hashes[out.result.Upload.Hash] {
			t.Fatal("distinct frontend source trees reused a hash")
		}
		hashes[out.result.Upload.Hash] = true
		if err := c.files.Hydrate(c.ctx, "demo", "fe", out.result.Upload.Hash); err != nil {
			t.Fatal(err)
		}
		payload, err := os.ReadFile(filepath.Join(c.files.FrontendDir("demo", out.result.Upload.Hash), "index.html"))
		if err != nil {
			t.Fatal(err)
		}
		if string(payload) != out.content {
			t.Fatalf("frontend payload%q want%q", payload, out.content)
		}
		if err := c.files.Hydrate(c.ctx, "demo", "be", out.result.Deploy.BeHash); err != nil {
			t.Fatal(err)
		}
		backend, err := os.ReadFile(filepath.Join(c.files.BackendDir("demo", out.result.Deploy.BeHash), "built.txt"))
		if err != nil || string(backend) != "backend-output" {
			t.Fatalf("backend%q error%v", backend, err)
		}
		row, err := c.database.GetDeployByID(out.result.Deploy.ID)
		if err != nil || row.Status != db.DeployReady {
			t.Fatalf("control deploy %+v error%v", row, err)
		}
	}
	if _, err := os.Stat(c.mirror); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control recreated its Git mirror: %v", err)
	}
}

type reservation struct {
	ID           string            `json:"id"`
	Status       string            `json:"status"`
	UploadURL    string            `json:"upload_url"`
	UploadFields map[string]string `json:"upload_fields"`
}

func reserve(t *testing.T, c *controlFixture, sha string, data []byte, configure ...func(*ingest.Request)) reservation {
	t.Helper()
	digest := sha256.Sum256(data)
	req := ingest.Request{RequestKey: rand.Text(), Ref: sha, Side: "frontend", ArchiveSize: int64(len(data)), ArchiveSHA256: hex.EncodeToString(digest[:])}
	for _, apply := range configure {
		apply(&req)
	}
	var result reservation
	if code := requestJSON(t, c.ctx, c.public.URL, "", "POST", "/api/repos/demo/ingestions", req, &result); code != 202 {
		t.Fatalf("reserve HTTP%d", code)
	}
	return result
}

func stage(t *testing.T, c *controlFixture, sha string, data []byte, configure ...func(*ingest.Request)) reservation {
	t.Helper()
	job := reserve(t, c, sha, data, configure...)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for key, value := range job.UploadFields {
		if err := form.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	part, err := form.CreateFormFile("file", "input.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, job.UploadURL, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("native upload HTTP%d", response.StatusCode)
	}
	if code := requestJSON(t, c.ctx, c.public.URL, "", "POST", "/api/repos/demo/ingestions/"+job.ID+"/submit", struct{}{}, nil); code != 202 {
		t.Fatalf("submit HTTP%d", code)
	}
	return job
}

func TestNativeS3LeaseRecoveryFencesStaleAndCrossJobCompletion(t *testing.T) {
	c := newControl(t, 4)
	defer c.cancel()
	sha := c.sourceRepo(t, "printf 'backend-output' > built.txt")
	_, data := archive(t, "recoverable payload")
	job := stage(t, c, sha, data)
	lost, err := c.database.ClaimJob("lost-worker", "expired-token", time.Now().Add(-2*time.Minute), time.Second, 3)
	if err != nil {
		t.Fatal(err)
	}
	if lost.ID != job.ID {
		t.Fatal("claimed wrong job")
	}
	if code := requestJSON(t, c.ctx, c.private.URL, "wrong-secret", "POST", "/v1/ingestions/claim", map[string]string{"worker_id": "unauthorized"}, nil); code != 401 {
		t.Fatalf("private wrong-secret HTTP%d", code)
	}
	var lease ingest.Lease
	if code := requestJSON(t, c.ctx, c.private.URL, workerSecret, "POST", "/v1/ingestions/claim", map[string]string{"worker_id": "replacement"}, &lease); code != 200 {
		t.Fatalf("replacement claim HTTP%d", code)
	}
	if lease.ID != job.ID || lease.Token == lost.LeaseToken {
		t.Fatal("expired lease was not replaced")
	}
	input, err := c.tier.OpenUpload(c.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	attempt, err := c.tier.Attempt(job.ID, lease.Token)
	if err != nil {
		t.Fatal(err)
	}
	executor := build.Executor{DataDir: t.TempDir(), Runner: build.HostRunner{}, MaxExtractBytes: 64 << 20}
	result, err := executor.Execute(c.ctx, lease.Spec, input, attempt)
	if err != nil {
		t.Fatal(err)
	}
	completion := ingest.Completion{Execution: result, Outputs: attempt.ListOutputs()}
	payload := map[string]any{"lease_token": lost.LeaseToken, "completion": completion}
	if code := requestJSON(t, c.ctx, c.private.URL, workerSecret, "POST", "/v1/ingestions/"+job.ID+"/complete", payload, nil); code != 409 {
		t.Fatalf("stale completion HTTP%d", code)
	}
	if len(completion.Outputs) == 0 {
		t.Fatal("worker produced no staged output")
	}
	changed := completion
	changed.Outputs = append([]s3store.Output(nil), completion.Outputs...)
	changed.Outputs[0].Key = s3store.StagingPrefix + "different-job/" + lease.Token + "/output.tar.zst"
	payload = map[string]any{"lease_token": lease.Token, "completion": changed}
	if code := requestJSON(t, c.ctx, c.private.URL, workerSecret, "POST", "/v1/ingestions/"+job.ID+"/complete", payload, nil); code != 400 {
		t.Fatalf("cross-job output HTTP%d", code)
	}
	reader, _, found, err := c.tier.Open(c.ctx, "demo", "fe", result.Upload.Hash)
	if reader != nil {
		reader.Close()
	}
	if err != nil || found {
		t.Fatalf("rejected completion published artifact: found%v error%v", found, err)
	}
	payload = map[string]any{"lease_token": lease.Token, "completion": completion}
	if code := requestJSON(t, c.ctx, c.private.URL, workerSecret, "POST", "/v1/ingestions/"+job.ID+"/complete", payload, nil); code != 202 {
		t.Fatalf("complete HTTP%d", code)
	}
	publishing, err := c.database.GetJob(job.ID)
	if err != nil || publishing.Status != db.JobPublishing {
		t.Fatalf("job%+v error%v", publishing, err)
	}
	reopened, err := db.Open(c.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := *c.coord
	restarted.Store = reopened
	publisherCtx, stop := context.WithCancel(c.ctx)
	done := make(chan struct{})
	go func() { defer close(done); restarted.Run(publisherCtx) }()
	defer func() { stop(); <-done }()
	waitFor(t, c.ctx, func() bool { state, err := c.database.GetJob(job.ID); return err == nil && state.Status == db.JobDone })
	if err := c.files.Hydrate(c.ctx, "demo", "fe", result.Upload.Hash); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(c.files.FrontendDir("demo", result.Upload.Hash), "index.html"))
	if err != nil || string(content) != "recoverable payload" {
		t.Fatalf("recovered content%q error%v", content, err)
	}
}

func TestNativeS3FullQueueRemainsHealthyAndClientCancellationIsBounded(t *testing.T) {
	c := newControl(t, 1)
	defer c.cancel()
	sha := c.sourceRepo(t, "printf built > built.txt")
	path, data := archive(t, "queued payload")
	held := reserve(t, c, sha, data)
	defer func() {
		if err := c.tier.DeleteUpload(context.Background(), held.ID); err != nil {
			t.Errorf("delete held upload: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(c.ctx, 250*time.Millisecond)
	defer cancel()
	_, err := client.New(c.public.URL, nil).StagedUpload(ctx, "demo", "frontend", "", sha, false, false, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queue wait did not respect deadline: %v", err)
	}
	healthy(t, c.ctx, c)
	state, err := c.database.GetJob(held.ID)
	if err != nil || state.Status != db.JobUploading {
		t.Fatalf("pending reservation changed %+v error%v", state, err)
	}
	stats, err := c.database.JobStats(time.Now())
	if err != nil || stats.Uploading != 1 || stats.Active != 0 || stats.Queued != 0 {
		t.Fatalf("queue exceeded reservation limit %+v error%v", stats, err)
	}
}

func TestNativeS3ArtifactsFinishAfterReadyAcrossControlRestart(t *testing.T) {
	for _, outcome := range []struct{ name, script, status string }{{"success", "printf 'downloadable-✓' > bundle.txt", db.ArtifactReady}, {"failure", "printf 'artifact failure' >&2; exit 17", db.ArtifactFailed}} {
		t.Run(outcome.name, func(t *testing.T) {
			c := newControl(t, 1)
			defer c.cancel()
			c.sourceRepo(t, "printf 'backend-output' > built.txt")
			gate := filepath.Join(t.TempDir(), "release-artifact")
			started := filepath.Join(t.TempDir(), "artifact-started")
			argv, _ := json.Marshal([][]string{{"sh", "-c", fmt.Sprintf("touch '%s'; while [ ! -f '%s' ]; do sleep 0.05; done; %s", started, gate, outcome.script)}})
			file, err := os.OpenFile(filepath.Join(c.source, "preview.toml"), os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := fmt.Fprintf(file, "\n[artifacts.bundle]\npath = \"backend\"\nbuild = %s\nfiles = [\"bundle.txt\"]\n", argv)
			if err := errors.Join(writeErr, file.Close()); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, c.source, "commit", "-qam", "add deferred artifact")
			sha := gittest.Run(t, c.source, "rev-parse", "HEAD")
			stopPublisher := c.startPublisher()
			c.startWorker(t, "artifact-worker")
			path, _ := archive(t, "<h1>ready before artifact</h1>")
			result, err := client.New(c.public.URL, nil).StagedUpload(c.ctx, "demo", "frontend", "", sha, false, true, path)
			if err != nil {
				t.Fatal(err)
			}
			if result.Deploy == nil || result.Deploy.Status != db.DeployReady {
				t.Fatalf("preview did not become ready %+v", result)
			}
			row, err := c.database.GetDeployByID(result.Deploy.ID)
			if err != nil {
				t.Fatal(err)
			}
			pending, ok := row.Artifacts["bundle"]
			if !ok || pending.Status != db.ArtifactBuilding {
				t.Fatalf("ready preview artifact state %+v", row.Artifacts)
			}
			if _, err := os.Stat(gate); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("artifact gate unexpectedly opened")
			}
			waitFor(t, c.ctx, func() bool { _, err := os.Stat(started); return err == nil })
			healthy(t, c.ctx, c)
			stats, err := c.database.JobStats(time.Now())
			if err != nil || stats.Active != 1 || stats.Publishing != 0 || stats.Queued != 0 {
				t.Fatalf("continuation failed with MaxPending=1: %+v error%v", stats, err)
			}
			stopPublisher()
			c.restartControl(t)
			c.startPublisher()
			healthy(t, c.ctx, c)
			if err := os.WriteFile(gate, []byte("continue"), 0600); err != nil {
				t.Fatal(err)
			}
			waitFor(t, c.ctx, func() bool {
				current, err := c.database.GetDeployByID(result.Deploy.ID)
				if err != nil {
					return false
				}
				return current.Artifacts["bundle"].Status == outcome.status
			})
			row, err = c.database.GetDeployByID(result.Deploy.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.Status != db.DeployReady {
				t.Fatalf("artifact %s changed preview status%s", outcome.name, row.Status)
			}
			artifact := row.Artifacts["bundle"]
			if outcome.status == db.ArtifactReady {
				if err := c.files.Hydrate(c.ctx, "demo", "dl", artifact.Hash); err != nil {
					t.Fatal(err)
				}
				content, err := os.ReadFile(filepath.Join(c.files.ArtifactDir("demo", artifact.Hash), "bundle.txt"))
				if err != nil || string(content) != "downloadable-✓" {
					t.Fatalf("artifact payload%q error%v", content, err)
				}
			} else if artifact.Error == "" {
				t.Fatal("artifact failure reason was lost")
			}
			if _, err := os.Stat(c.mirror); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("control Git mirror recreated: %v", err)
			}
		})
	}
}

func TestNativeS3LegacyDeployRequestUsesWorkers(t *testing.T) {
	c := newControl(t, 4)
	defer c.cancel()
	c.sourceRepo(t, "printf 'native-backend' > built.txt")
	manifestPath := filepath.Join(c.source, "preview.toml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = bytes.ReplaceAll(manifest, []byte("exit 91"), []byte("mkdir -p dist; printf 'native-frontend' > dist/index.html"))
	if err := os.WriteFile(manifestPath, manifest, 0644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, c.source, "commit", "-qam", "enable frontend build")
	sha := gittest.Run(t, c.source, "rev-parse", "HEAD")
	if code := requestJSON(t, c.ctx, c.public.URL, "", "POST", "/api/repos/demo/uploads/frontend?ref="+sha, map[string]string{}, nil); code != 426 {
		t.Fatalf("synchronous upload bypass HTTP%d", code)
	}
	c.startPublisher()
	c.startWorker(t, "legacy-request-worker")
	result, err := client.New(c.public.URL, nil).CreateDeploy(c.ctx, "demo", sha, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != db.DeployReady || result.SHA != sha || result.PreviewURL == "" {
		t.Fatalf("legacy request did not finish remotely %+v", result)
	}
	cached, err := client.New(c.public.URL, nil).CreateDeploy(c.ctx, "demo", sha, false)
	if err != nil || cached.ID != result.ID || cached.FeHash != result.FeHash {
		t.Fatalf("cached remote deploy %+v error%v", cached, err)
	}

	for _, side := range []struct{ name, hash, file, content string }{{"fe", result.FeHash, "index.html", "native-frontend"}, {"be", result.BeHash, "built.txt", "native-backend"}} {
		if err := c.files.Hydrate(c.ctx, "demo", side.name, side.hash); err != nil {
			t.Fatal(err)
		}
		path := c.files.FrontendDir("demo", side.hash)
		if side.name == "be" {
			path = c.files.BackendDir("demo", side.hash)
		}
		content, err := os.ReadFile(filepath.Join(path, side.file))
		if err != nil || string(content) != side.content {
			t.Fatalf("remote side%s content%q error%v", side.name, content, err)
		}
	}
	if _, err := os.Stat(c.mirror); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy request recreated control Git: %v", err)
	}
}

func TestNativeS3SupersededArtifactJobCannotReplaceNewGeneration(t *testing.T) {
	c := newControl(t, 2)
	defer c.cancel()
	sha := c.sourceRepo(t, "printf built > built.txt")
	digest := sha256.Sum256([]byte("same-artifact-inputs"))
	hash := hex.EncodeToString(digest[:])
	newDir, oldDir := t.TempDir(), t.TempDir()
	for dir, content := range map[string]string{newDir: "new generation", oldDir: "old generation"} {
		if err := os.WriteFile(filepath.Join(dir, "bundle.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.tier.Save(c.ctx, "demo", "dl", hash, newDir); err != nil {
		t.Fatal(err)
	}
	currentRef := db.ArtifactRef{Hash: hash, Status: db.ArtifactReady, LogPath: filepath.Join(c.root, "new-generation.log")}
	if err := os.WriteFile(currentRef.LogPath, []byte("new generation log"), 0600); err != nil {
		t.Fatal(err)
	}
	row, err := c.database.FinalizeRemoteDeploy(c.repo.ID, db.RemoteDeploy{SHA: sha, JobID: "new-parent", Artifacts: map[string]db.ArtifactRef{"bundle": currentRef}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := ingest.Snapshot{ParentID: "old-parent", Execution: build.ExecutionSpec{RepoName: "demo", Source: c.repo.Source, Ref: sha, ArtifactsOnly: true, Rebuild: true, ExpectedArtifacts: map[string]db.ArtifactRef{"bundle": {Hash: hash}}}}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := c.database.ReserveJob(db.JobSpec{ID: "superseded-child", RepoID: c.repo.ID, RequestKey: "old-artifact-child", Fingerprint: "old-generation", Kind: db.JobBuild, SpecJSON: string(data), ExpiresAt: time.Now().Add(time.Minute).Unix()}, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.database.ClaimJob("old-worker", "old-token", time.Now(), time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := c.tier.Attempt(job.ID, lease.LeaseToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.Save(c.ctx, "demo", "dl", hash, oldDir); err != nil {
		t.Fatal(err)
	}
	completion := ingest.Completion{Execution: build.ExecutionResult{Artifacts: &build.ExecutedArtifacts{SHA: sha, Artifacts: map[string]db.ArtifactRef{"bundle": {Hash: hash, Status: db.ArtifactReady}}, Logs: map[string]string{"bundle": "old generation log"}}}, Outputs: attempt.ListOutputs()}
	if err := c.coord.Complete(job.ID, lease.LeaseToken, completion); err != nil {
		t.Fatal(err)
	}
	c.startPublisher()
	waitFor(t, c.ctx, func() bool { job, err := c.database.GetJob(job.ID); return err == nil && job.Status == db.JobDone })
	after, err := c.database.GetDeployByID(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.IngestionJobID != "new-parent" || after.Artifacts["bundle"] != currentRef {
		t.Errorf("superseded artifact changed current deploy: %+v", after)
	}
	logs, err := os.ReadFile(currentRef.LogPath)
	if err != nil || string(logs) != "new generation log" {
		t.Fatalf("current logs changed: %q error%v", logs, err)
	}
	if err := c.files.Hydrate(c.ctx, "demo", "dl", hash); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(c.files.ArtifactDir("demo", hash), "bundle.txt"))
	if err != nil || string(content) != "new generation" {
		t.Fatalf("superseded output replaced canonical bytes: %q error%v", content, err)
	}
}

func TestNativeS3OlderParentCannotReplaceNewRebuild(t *testing.T) {
	c := newControl(t, 8)
	defer c.cancel()
	sha := c.sourceRepo(t, "printf built > built.txt")
	digest := sha256.Sum256([]byte("same frontend inputs"))
	hash := hex.EncodeToString(digest[:])
	snapshot := ingest.Snapshot{Execution: build.ExecutionSpec{RepoName: "demo", Source: c.repo.Source, Ref: sha, Deploy: true, Rebuild: true}}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	reserveParent := func(id, token, content string) (db.Job, ingest.Completion) {
		t.Helper()
		job, _, err := c.database.ReserveJob(db.JobSpec{ID: id, RepoID: c.repo.ID, RequestKey: id, Fingerprint: id, Kind: db.JobBuild, SpecJSON: string(data), ExpiresAt: time.Now().Add(time.Minute).Unix()}, 8, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		lease, err := c.database.ClaimJob(id, token, time.Now(), time.Minute, 3)
		if err != nil {
			t.Fatal(err)
		}
		if lease.ID != job.ID {
			t.Fatal("claimed wrong parent")
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		attempt, err := c.tier.Attempt(job.ID, token)
		if err != nil {
			t.Fatal(err)
		}
		if err := attempt.Save(c.ctx, "demo", "fe", hash, dir); err != nil {
			t.Fatal(err)
		}
		return job, ingest.Completion{Execution: build.ExecutionResult{Deploy: &build.ExecutedDeploy{SHA: sha, FrontendHash: hash, Logs: map[string]string{"fe": content}}}, Outputs: attempt.ListOutputs()}
	}
	old, oldResult := reserveParent("z-old-parent", "old-parent-token", "old parent")
	newer, newResult := reserveParent("a-new-parent", "new-parent-token", "new parent")
	if err := c.coord.Complete(newer.ID, "new-parent-token", newResult); err != nil {
		t.Fatal(err)
	}
	c.startPublisher()
	waitFor(t, c.ctx, func() bool { job, err := c.database.GetJob(newer.ID); return err == nil && job.Status == db.JobDone })
	if err := c.coord.Complete(old.ID, "old-parent-token", oldResult); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c.ctx, func() bool { job, err := c.database.GetJob(old.ID); return err == nil && job.Status == db.JobDone })
	row, err := c.database.GetDeployBySHA(c.repo.ID, sha)
	if err != nil {
		t.Fatal(err)
	}
	if row.IngestionJobID != newer.ID || row.AttemptCount != 1 {
		t.Errorf("old parent replaced generation: %+v", row)
	}
	logs, err := os.ReadFile(row.FeBuildLogPath)
	if err != nil || !bytes.HasSuffix(logs, []byte("new parent")) {
		t.Errorf("old parent replaced logs: %q error%v", logs, err)
	}
	if err := c.files.Hydrate(c.ctx, "demo", "fe", hash); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(c.files.FrontendDir("demo", hash), "index.html"))
	if err != nil || string(content) != "new parent" {
		t.Errorf("old parent replaced canonical bytes: %q error%v", content, err)
	}
	finished, err := c.database.GetJob(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.coord.Result(c.ctx, finished)
	if err != nil || result.Deploy == nil || result.Deploy.IngestionJobID != newer.ID {
		t.Fatalf("old job did not resolve current deploy: %+v error%v", result, err)
	}
}

func TestNativeS3ArtifactRepairKeepsParentSnapshotAndContinuationIdentity(t *testing.T) {
	c := newControl(t, 3)
	defer c.cancel()
	sha := c.sourceRepo(t, "printf built > built.txt")
	c.coord.LocalManifestDir = t.TempDir()
	manifestPath := filepath.Join(c.coord.LocalManifestDir, "demo.toml")
	originalManifest := []byte("# original parent manifest\n")
	if err := os.WriteFile(manifestPath, originalManifest, 0600); err != nil {
		t.Fatal(err)
	}
	parent, err := c.coord.ReserveBuild(c.ctx, "demo", sha, true, "rebuild-actor")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.database.ClaimJob("parent-worker", "parent-token", time.Now(), time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("artifact inputs"))
	hash := hex.EncodeToString(digest[:])
	refs := map[string]db.ArtifactRef{"cli": {Hash: hash, Status: db.ArtifactBuilding}, "sdk": {Hash: hash, Status: db.ArtifactBuilding}}
	if err := c.coord.Complete(parent.ID, lease.LeaseToken, ingest.Completion{Execution: build.ExecutionResult{Deploy: &build.ExecutedDeploy{SHA: sha, Artifacts: refs}}}); err != nil {
		t.Fatal(err)
	}
	stop := c.startPublisher()
	waitFor(t, c.ctx, func() bool { job, err := c.database.GetJob(parent.ID); return err == nil && job.Status == db.JobDone })
	stop()
	child, err := c.database.ClaimJob("child-worker", "child-token", time.Now(), time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("# changed after parent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	row, err := c.database.GetDeployBySHA(c.repo.ID, sha)
	if err != nil {
		t.Fatal(err)
	}
	partial := row.Artifacts["cli"]
	partial.Status = db.ArtifactReady
	row.Artifacts["cli"] = partial
	if err := c.database.SetDeployArtifacts(row.ID, row.Artifacts); err != nil {
		t.Fatal(err)
	}
	readyRow, err := c.database.GetDeployByID(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.coord.ScheduleArtifactsForDeploy(c.ctx, readyRow); err != nil {
		t.Fatal(err)
	}
	stats, err := c.database.JobStats(time.Now())
	if err != nil || stats.Active != 1 || stats.Queued != 0 {
		t.Errorf("repair duplicated continuation: %+v error%v", stats, err)
	}
	var snapshot ingest.Snapshot
	if err := json.Unmarshal([]byte(child.SpecJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ParentID != parent.ID || !snapshot.Execution.Rebuild || !bytes.Equal(snapshot.Execution.LocalManifest, originalManifest) || len(snapshot.Execution.ExpectedArtifacts) != 2 {
		t.Errorf("child lost immutable parent snapshot: %+v", snapshot)
	}
	if snapshot.CreatedBy != "" || snapshot.Execution.Overwrite || snapshot.Execution.Side != "" || snapshot.Execution.Name != "" {
		t.Errorf("child identity contains unrelated parent fields: %+v", snapshot)
	}
}

func TestNativeS3ReadyParentFinishesWhenArtifactAdmissionIsFull(t *testing.T) {
	c := newControl(t, 1)
	defer c.cancel()
	sha := c.sourceRepo(t, "printf built > built.txt")
	parent, err := c.coord.ReserveBuild(c.ctx, "demo", sha, false, "actor")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.database.ClaimJob("parent-worker", "parent-token", time.Now(), time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.database.ReserveContinuation(db.JobSpec{ID: "continuation-slot-busy", RepoID: c.repo.ID, RequestKey: "occupied-slot", Fingerprint: "occupied", Kind: db.JobBuild, SpecJSON: parent.SpecJSON, ExpiresAt: time.Now().Add(time.Minute).Unix()}, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("artifact inputs"))
	hash := hex.EncodeToString(digest[:])
	completion := ingest.Completion{Execution: build.ExecutionResult{Deploy: &build.ExecutedDeploy{SHA: sha, Artifacts: map[string]db.ArtifactRef{"cli": {Hash: hash, Status: db.ArtifactBuilding}}}}}
	if err := c.coord.Complete(parent.ID, lease.LeaseToken, completion); err != nil {
		t.Fatal(err)
	}
	c.startPublisher()
	deadline, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	waitFor(t, deadline, func() bool { job, err := c.database.GetJob(parent.ID); return err == nil && job.Status == db.JobDone })
	row, err := c.database.GetDeployBySHA(c.repo.ID, sha)
	if err != nil || row.Status != db.DeployReady || row.Artifacts["cli"].Status != db.ArtifactBuilding {
		t.Fatalf("ready parent state %+v error%v", row, err)
	}
	waitFor(t, c.ctx, func() bool {
		stats, err := c.database.JobStats(time.Now())
		return err == nil && stats.Queued == 2 && stats.Publishing == 0
	})
}
