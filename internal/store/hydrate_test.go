package store

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// tarDirRaw tars a directory's contents (relative paths, no compression) so a
// fake tier's Open can reproduce the tree via ExtractTar.
func tarDirRaw(t *testing.T, srcDir string) []byte {
	t.Helper()
	root, err := filepath.Abs(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o755, Typeflag: tar.TypeDir})
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeTier is an in-memory ArtifactTier for store tests.
type fakeTier struct {
	mu    sync.Mutex
	blobs map[string][]byte
	opens int32 // count of Open calls, to observe singleflight dedup
}

func fkey(repo, side, hash string) string { return repo + "/" + side + "/" + hash }

func (f *fakeTier) put(repo, side, hash string, blob []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blobs == nil {
		f.blobs = map[string][]byte{}
	}
	f.blobs[fkey(repo, side, hash)] = blob
}

func (f *fakeTier) Save(_ context.Context, repo, side, hash, srcDir string) error {
	return nil
}

func (f *fakeTier) Open(_ context.Context, repo, side, hash string) (io.ReadCloser, bool, error) {
	atomic.AddInt32(&f.opens, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blobs[fkey(repo, side, hash)]
	if !ok {
		return nil, false, nil
	}
	return io.NopCloser(bytes.NewReader(b)), true, nil
}

func newStore(t *testing.T) *Store {
	root := t.TempDir()
	return New(filepath.Join(root, "artifacts"), filepath.Join(root, "state"), filepath.Join(root, "tmp"))
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHydrateFillsMissingSide(t *testing.T) {
	s := newStore(t)
	tier := &fakeTier{}
	s.SetArtifactTier(tier)

	// Stage a source tree, tar it into the fake tier as fe/abc.
	src := t.TempDir()
	writeTree(t, src, map[string]string{"index.html": "hello", "assets/app.js": "x"})
	tier.put("demo", "fe", "abc", tarDirRaw(t, src))

	if s.HasFrontend("demo", "abc") {
		t.Fatal("precondition: should not be resident yet")
	}
	if err := s.Hydrate(context.Background(), "demo", "fe", "abc"); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if !s.HasFrontend("demo", "abc") {
		t.Fatal("side was not hydrated into the local store")
	}
	got, err := os.ReadFile(filepath.Join(s.FrontendDir("demo", "abc"), "assets/app.js"))
	if err != nil || string(got) != "x" {
		t.Fatalf("hydrated content = %q, %v", got, err)
	}
}

func TestHydrateAlreadyResidentIsNoOp(t *testing.T) {
	s := newStore(t)
	s.SetArtifactTier(&fakeTier{}) // tier present but object absent
	// Publish a resident side directly.
	src := t.TempDir()
	writeTree(t, src, map[string]string{"f": "1"})
	if err := s.PublishFrontend("demo", "abc", src, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Hydrate(context.Background(), "demo", "fe", "abc"); err != nil {
		t.Fatalf("hydrate of resident side should be a no-op, got %v", err)
	}
}

func TestHydrateNotInTier(t *testing.T) {
	s := newStore(t)
	s.SetArtifactTier(&fakeTier{})
	err := s.Hydrate(context.Background(), "demo", "be", "missing")
	if !errors.Is(err, ErrNotInTier) {
		t.Fatalf("want ErrNotInTier, got %v", err)
	}
}

func TestHydrateNoTierConfigured(t *testing.T) {
	s := newStore(t)
	err := s.Hydrate(context.Background(), "demo", "be", "abc")
	if err == nil || errors.Is(err, ErrNotInTier) {
		t.Fatalf("want a 'no tier' error, got %v", err)
	}
}

func TestHydrateConcurrentDedup(t *testing.T) {
	s := newStore(t)
	tier := &fakeTier{}
	s.SetArtifactTier(tier)
	src := t.TempDir()
	writeTree(t, src, map[string]string{"f": "data"})
	tier.put("demo", "be", "abc", tarDirRaw(t, src))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Hydrate(context.Background(), "demo", "be", "abc")
		}()
	}
	wg.Wait()
	if !s.HasBackend("demo", "abc") {
		t.Fatal("not hydrated")
	}
	// singleflight should have collapsed the concurrent fetches to far fewer
	// than 8 opens (typically 1); assert it did not fan out to one per caller.
	if n := atomic.LoadInt32(&tier.opens); n >= 8 {
		t.Fatalf("expected deduplicated opens, got %d", n)
	}
}
