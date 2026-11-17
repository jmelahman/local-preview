package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/githuboidc"
	"github.com/jmelahman/local-preview/internal/githubsso"
	"github.com/jmelahman/local-preview/internal/ingest"
)

type ingestionSSO struct {
	fakeSSO
	calls int
}

func (s *ingestionSSO) VerifyToken(ctx context.Context, token string) (githubsso.Identity, error) {
	s.calls++
	return s.fakeSSO.VerifyToken(ctx, token)
}

type ingestionVerifier struct{ calls int }

func (v *ingestionVerifier) Verify(_ context.Context, token string) (githuboidc.Claims, error) {
	v.calls++
	if token != "own-token" && token != "other-token" {
		return githuboidc.Claims{}, errors.New("invalid token")
	}
	repo := "owner/app"
	if token == "other-token" {
		repo = "other/app"
	}
	return githuboidc.Claims{Repository: repo, Actor: "workflow-user"}, nil
}

func TestIngestionRoutesBindEveryOIDCRequest(t *testing.T) {
	d := ssoDeps(t)
	verifier := &ingestionVerifier{}
	d.UploadAuth = verifier
	sso := &ingestionSSO{identity: githubsso.Identity{Login: "member", UserID: 1}}
	d.SSO = sso
	if _, err := d.Store.CreateRepo("demo", "https://github.com/owner/app", "unused", db.RepoReady); err != nil {
		t.Fatal(err)
	}
	handler := AuthMiddleware(d, NewMux(d))
	routes := [][2]string{{"POST", "/api/repos/demo/ingestions"}, {"POST", "/api/repos/demo/ingestions/job/submit"}, {"GET", "/api/repos/demo/ingestions/job"}, {"DELETE", "/api/repos/demo/ingestions/job"}}
	for _, route := range routes {
		for _, test := range []struct {
			token  string
			status int
		}{{"", 401}, {"bad", 401}, {"other-token", 403}, {"own-token", 503}, {"goodtoken", 503}} {
			t.Run(route[0]+route[1]+test.token, func(t *testing.T) {
				req := httptest.NewRequest(route[0], route[1], strings.NewReader("{}"))
				if test.token != "" {
					req.Header.Set("Authorization", "Bearer "+test.token)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != test.status {
					t.Fatalf("got%d want%d: %s", rec.Code, test.status, rec.Body)
				}
			})
		}
	}
	if verifier.calls != 16 {
		t.Fatalf("verifier calls%d want16, one fresh verification per bearer request", verifier.calls)
	}
	if sso.calls != 2 {
		t.Fatalf("SSO verification calls%d want2; valid OIDC requests must not contact the PAT provider", sso.calls)
	}
}

func TestIngestionDirectRoutesRequireOIDCBeforeRepoLookup(t *testing.T) {
	verifier := &ingestionVerifier{}
	d := Deps{UploadAuth: verifier}
	for _, route := range [][2]string{{"POST", "/api/repos/unknown/ingestions"}, {"POST", "/api/repos/unknown/ingestions/id/submit"}, {"GET", "/api/repos/unknown/ingestions/id"}, {"DELETE", "/api/repos/unknown/ingestions/id"}} {
		rec := httptest.NewRecorder()
		NewMux(d).ServeHTTP(rec, httptest.NewRequest(route[0], route[1], strings.NewReader("{}")))
		if rec.Code != 401 {
			t.Fatalf("got%d: %s", rec.Code, rec.Body)
		}
	}
	if verifier.calls != 0 {
		t.Fatal("verified absent tokens")
	}
}

func TestIngestionOIDCDoesNotAuthorizeOtherRepoRoutes(t *testing.T) {
	d := ssoDeps(t)
	d.UploadAuth = &ingestionVerifier{}
	handler := AuthMiddleware(d, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected authenticated route")
		w.WriteHeader(http.StatusOK)
	}))
	for _, route := range [][2]string{{"GET", "/api/repos/demo/ingestions"}, {"POST", "/api/repos/demo/ingestions/job"}, {"POST", "/api/repos/demo/ingestions/job/retry"}, {"GET", "/api/repos/demo/ingestions/job/logs"}, {"DELETE", "/api/repos/demo"}} {
		req := httptest.NewRequest(route[0], route[1], nil)
		req.Header.Set("Authorization", "Bearer own-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("%s%s got%d", route[0], route[1], rec.Code)
		}
	}
}

func TestIngestionErrorsDoNotExposeInternalDetails(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		retry  string
	}{{db.ErrJobFull, 429, "5"}, {db.ErrJobConflict, 409, ""}, {db.ErrJobNotReady, 409, ""}, {db.ErrNotFound, 404, ""}, {errors.New("credential=private-value"), 500, ""}} {
		rec := httptest.NewRecorder()
		ingestionError(rec, "test", test.err)
		if rec.Code != test.status || rec.Header().Get("Retry-After") != test.retry {
			t.Fatalf("status%d retry%s", rec.Code, rec.Header().Get("Retry-After"))
		}
		var payload map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(payload["error"], "private-value") {
			t.Fatal("internal error leaked")
		}
	}
}

func TestIngestionStatusAndActionsCannotCrossRepoID(t *testing.T) {
	d := ssoDeps(t)
	d.UploadAuth = &ingestionVerifier{}
	repo, err := d.Store.CreateRepo("demo", "https://github.com/owner/app", "unused", db.RepoReady)
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.Store.CreateRepo("other", "https://github.com/other/app", "unused", db.RepoReady)
	if err != nil {
		t.Fatal(err)
	}
	d.Ingestion = &ingest.Coordinator{Store: d.Store}
	now := time.Now()
	for _, spec := range []db.JobSpec{{ID: "own", RepoID: repo.ID}, {ID: "foreign", RepoID: other.ID}} {
		spec.RequestKey = "request-" + spec.ID
		spec.Fingerprint = "fingerprint"
		spec.Kind = db.JobUpload
		spec.SpecJSON = `{"private_source":"credential=private-value"}`
		spec.ExpiresAt = now.Add(time.Hour).Unix()
		if _, _, err := d.Store.ReserveJob(spec, 10, now); err != nil {
			t.Fatal(err)
		}
	}
	handler := AuthMiddleware(d, NewMux(d))
	for _, route := range [][2]string{{"GET", "/api/repos/demo/ingestions/foreign"}, {"POST", "/api/repos/demo/ingestions/foreign/submit"}, {"DELETE", "/api/repos/demo/ingestions/foreign"}} {
		req := httptest.NewRequest(route[0], route[1], strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer own-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("%s %s got%d: %s", route[0], route[1], rec.Code, rec.Body)
		}
	}
	foreign, err := d.Store.GetJob("foreign")
	if err != nil {
		t.Fatal(err)
	}
	if foreign.Status != db.JobUploading {
		t.Fatal("foreign job mutated")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/repos/demo/ingestions/own", nil)
	req.Header.Set("Authorization", "Bearer own-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("own job got%d: %s", rec.Code, rec.Body)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 2 || string(payload["id"]) != `"own"` || string(payload["status"]) != `"uploading"` {
		t.Fatalf("unexpected job fields%s", rec.Body)
	}
}

func TestIngestionSessionPreservesOriginCheck(t *testing.T) {
	d := ssoDeps(t)
	d.UploadAuth = &ingestionVerifier{}
	setup := httptest.NewRecorder()
	if err := d.startSession(setup, githubsso.Identity{Login: "member", UserID: 1}); err != nil {
		t.Fatal(err)
	}
	cookie := findCookie(setup.Result().Cookies(), sessionCookieName)
	handler := AuthMiddleware(d, NewMux(d))
	for _, test := range []struct {
		origin string
		status int
	}{{"https://foreign.example", 403}, {d.DashboardOrigin, 503}} {
		req := httptest.NewRequest(http.MethodPost, "/api/repos/demo/ingestions", strings.NewReader("{}"))
		req.AddCookie(cookie)
		req.Header.Set("Origin", test.origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Fatalf("origin%s got%d: %s", test.origin, rec.Code, rec.Body)
		}
	}
}

func TestIngestionConfiguredRejectsLegacyUploadBeforeGit(t *testing.T) {
	d := ssoDeps(t)
	d.UploadAuth = &ingestionVerifier{}
	d.Ingestion = &ingest.Coordinator{Store: d.Store}
	if _, err := d.Store.CreateRepo("demo", "https://github.com/owner/app", "no-mirror", db.RepoReady); err != nil {
		t.Fatal(err)
	}
	handler := AuthMiddleware(d, NewMux(d))
	for _, path := range []string{"/api/repos/demo/uploads/frontend", "/api/repos/demo/uploads/backend", "/api/repos/demo/uploads/artifacts/download"} {
		for _, tc := range []struct {
			token string
			want  int
		}{{"", 401}, {"other-token", 403}, {"own-token", 426}} {
			req := httptest.NewRequest(http.MethodPost, path+"?ref=main", strings.NewReader("not an archive"))
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s got%d want%d: %s", path, rec.Code, tc.want, rec.Body)
			}
			if rec.Code == 426 && !strings.Contains(rec.Body.String(), "--staged") {
				t.Fatal("upgrade response omitted the staged command")
			}
		}
	}
}

func TestIngestionConfiguredDeployQueuesWithoutControlGit(t *testing.T) {
	d := ssoDeps(t)
	d.UploadAuth = &ingestionVerifier{}
	d.Ingestion = &ingest.Coordinator{Store: d.Store, MaxPending: 4}
	repo, err := d.Store.CreateRepo("demo", "https://github.com/owner/app", "no-mirror", db.RepoReady)
	if err != nil {
		t.Fatal(err)
	}
	handler := AuthMiddleware(d, NewMux(d))
	req := httptest.NewRequest(http.MethodPost, "/api/deploys", strings.NewReader(`{"repo":"demo","ref":"new-branch","rebuild":true}`))
	req.Header.Set("Authorization", "Bearer own-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("got%d: %s", rec.Code, rec.Body)
	}
	var accepted struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Repo   string `json:"repo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.ID == "" || accepted.Status != db.JobQueued || accepted.Repo != "demo" {
		t.Fatalf("invalid accepted job%s", rec.Body)
	}
	job, err := d.Store.GetJob(accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot ingest.Snapshot
	if err := json.Unmarshal([]byte(job.SpecJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if job.RepoID != repo.ID || job.Kind != db.JobBuild || snapshot.Execution.Side != "" || !snapshot.Execution.Deploy || !snapshot.Execution.Rebuild || snapshot.Execution.Ref != "new-branch" || snapshot.CreatedBy != "workflow-user" {
		t.Fatalf("invalid queued job %+v snapshot%+v", job, snapshot)
	}
}

func TestIngestionBuildRequiresOIDCWhenSSOIsDisabled(t *testing.T) {
	d := Deps{UploadAuth: &ingestionVerifier{}, Ingestion: &ingest.Coordinator{}}
	req := httptest.NewRequest(http.MethodPost, "/api/deploys", strings.NewReader(`{"repo":"demo","ref":"main"}`))
	rec := httptest.NewRecorder()
	AuthMiddleware(d, NewMux(d)).ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("configured ingestion accepted an unauthenticated deploy: HTTP%d %s", rec.Code, rec.Body)
	}
}
