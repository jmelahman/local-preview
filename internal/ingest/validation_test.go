package ingest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/db"
)

func TestCompletionMatchesReservedResultMode(t *testing.T) {
	sha, hash := strings.Repeat("a", 40), strings.Repeat("b", 64)
	tests := []struct {
		name   string
		spec   build.ExecutionSpec
		result build.ExecutionResult
		valid  bool
	}{
		{"direct build", build.ExecutionSpec{Ref: sha, Deploy: true}, build.ExecutionResult{Deploy: &build.ExecutedDeploy{SHA: sha}}, true},
		{"wrong direct commit", build.ExecutionSpec{Ref: sha, Deploy: true}, build.ExecutionResult{Deploy: &build.ExecutedDeploy{SHA: strings.Repeat("c", 40)}}, false},
		{"unexpected upload", build.ExecutionSpec{Ref: sha, Deploy: true}, build.ExecutionResult{Upload: &build.UploadResult{SHA: sha, Hash: hash, Side: build.SideFrontend}, Deploy: &build.ExecutedDeploy{SHA: sha}}, false},
		{"upload only", build.ExecutionSpec{Ref: sha, Side: build.SideFrontend}, build.ExecutionResult{Upload: &build.UploadResult{SHA: sha, Hash: hash, Side: build.SideFrontend}}, true},
		{"unexpected deploy", build.ExecutionSpec{Ref: sha, Side: build.SideFrontend}, build.ExecutionResult{Upload: &build.UploadResult{SHA: sha, Hash: hash, Side: build.SideFrontend}, Deploy: &build.ExecutedDeploy{SHA: sha}}, false},
		{"artifact child", build.ExecutionSpec{Ref: sha, ArtifactsOnly: true, ExpectedArtifacts: map[string]db.ArtifactRef{"cli": {Hash: hash}}}, build.ExecutionResult{Artifacts: &build.ExecutedArtifacts{SHA: sha, Artifacts: map[string]db.ArtifactRef{"cli": {Hash: hash, Status: db.ArtifactFailed}}}}, true},
		{"missing artifact", build.ExecutionSpec{Ref: sha, ArtifactsOnly: true, ExpectedArtifacts: map[string]db.ArtifactRef{"cli": {Hash: hash}}}, build.ExecutionResult{Artifacts: &build.ExecutedArtifacts{SHA: sha}}, false},
		{"partial artifacts", build.ExecutionSpec{Ref: sha, ArtifactsOnly: true, ExpectedArtifacts: map[string]db.ArtifactRef{"cli": {Hash: hash}, "sdk": {Hash: hash}}}, build.ExecutionResult{Artifacts: &build.ExecutedArtifacts{SHA: sha, Artifacts: map[string]db.ArtifactRef{"cli": {Hash: hash, Status: db.ArtifactReady}}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(Snapshot{Execution: tt.spec})
			if err != nil {
				t.Fatal(err)
			}
			job := db.Job{SpecJSON: string(data), Status: db.JobLeased, LeaseToken: "token"}
			err = validateCompletion(job, "token", Completion{Execution: tt.result})
			if tt.valid && err != nil {
				t.Fatal(err)
			}
			if !tt.valid && !errors.Is(err, ErrValidation) {
				t.Fatalf("invalid completion accepted: %v", err)
			}
		})
	}
}

func TestArtifactCompletionLogsMatchDeclaredArtifacts(t *testing.T) {
	sha, hash := strings.Repeat("a", 40), strings.Repeat("b", 64)
	spec := build.ExecutionSpec{Ref: sha, ArtifactsOnly: true, ExpectedArtifacts: map[string]db.ArtifactRef{"cli": {Hash: hash}}}
	artifacts := &build.ExecutedArtifacts{SHA: sha, Artifacts: map[string]db.ArtifactRef{"cli": {Hash: hash, Status: db.ArtifactReady}}}
	for _, tt := range []struct {
		name  string
		logs  map[string]string
		valid bool
	}{
		{"bounded tail", map[string]string{"cli": strings.Repeat("x", 512<<10)}, true},
		{"over budget", map[string]string{"cli": strings.Repeat("x", (512<<10)+1)}, false},
		{"unknown artifact", map[string]string{"other": "tail"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			artifacts.Logs = tt.logs
			if got := validArtifactResult(spec, artifacts); got != tt.valid {
				t.Fatalf("valid result=%v, want %v", got, tt.valid)
			}
		})
	}
}
