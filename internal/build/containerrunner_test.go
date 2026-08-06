package build

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmelahman/local-preview/internal/dockerapi"
)

func specFor(t *testing.T, image string, argv ...string) RunSpec {
	t.Helper()
	scratch := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scratch, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	return RunSpec{
		RepoName:   "demo",
		SHA:        "deadbeef",
		ScratchDir: scratch,
		Dir:        "web",
		Argv:       argv,
		Image:      image,
	}
}

func TestAutoRunnerHostPath(t *testing.T) {
	r := &autoRunner{}
	spec := specFor(t, "", "sh", "-c", "echo host-built > out.txt")
	var out bytes.Buffer
	if err := r.Run(context.Background(), spec, &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(spec.ScratchDir, "web", "out.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "host-built" {
		t.Fatalf("host output: %q, %v", b, err)
	}
}

func TestAutoRunnerFallsBackWithoutDocker(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	r := &autoRunner{}
	spec := specFor(t, "alpine:3.20", "sh", "-c", "echo fallback > out.txt")
	var out bytes.Buffer
	if err := r.Run(context.Background(), spec, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "warning: build image") {
		t.Fatalf("missing fallback warning: %q", out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(spec.ScratchDir, "web", "out.txt")); strings.TrimSpace(string(b)) != "fallback" {
		t.Fatalf("fallback did not run on host: %q", b)
	}
}

// TestAutoRunnerContainer runs a real one-shot build container. Skipped when
// no daemon is reachable (e.g. CI).
func TestAutoRunnerContainer(t *testing.T) {
	ctx := context.Background()
	if _, err := dockerapi.Connect(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	r := &autoRunner{}
	spec := specFor(t, "alpine:3.20", "sh", "-c", "pwd && echo containerized > out.txt")
	var out bytes.Buffer
	if err := r.Run(ctx, spec, &out); err != nil {
		t.Fatalf("Run: %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "/preview-build/web") {
		t.Fatalf("workdir missing from output: %q", out.String())
	}
	b, err := os.ReadFile(filepath.Join(spec.ScratchDir, "web", "out.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "containerized" {
		t.Fatalf("container output on host: %q, %v", b, err)
	}

	// Failures propagate with output.
	out.Reset()
	err = r.Run(ctx, specFor(t, "alpine:3.20", "sh", "-c", "echo boom >&2; exit 7"), &out)
	if err == nil || !strings.Contains(err.Error(), "status 7") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "boom") {
		t.Fatalf("stderr missing: %q", out.String())
	}
}
