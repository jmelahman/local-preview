// Package store owns on-disk content-addressed artifacts and mutable backend
// state directories.
//
// Every publish builds into tmp/ and lands with a same-filesystem os.Rename,
// so "the directory exists" always means "the content is complete" — a crash
// mid-build can never leave a half-written artifact behind. tmp/ is never
// authoritative and is swept at startup.
package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Store manages artifacts under artifactsDir, state under stateDir, and
// staging under tmpDir. All three must live on the same filesystem so
// renames are atomic.
type Store struct {
	artifactsDir string
	stateDir     string
	tmpDir       string
}

// New returns a Store over the three roots (created lazily).
func New(artifactsDir, stateDir, tmpDir string) *Store {
	return &Store{artifactsDir: artifactsDir, stateDir: stateDir, tmpDir: tmpDir}
}

// FrontendDir returns the artifact path for a frontend hash.
func (s *Store) FrontendDir(repo, hash string) string {
	return filepath.Join(s.artifactsDir, repo, "fe", hash)
}

// BackendDir returns the artifact path for a backend hash.
func (s *Store) BackendDir(repo, hash string) string {
	return filepath.Join(s.artifactsDir, repo, "be", hash)
}

// HasFrontend reports whether the frontend artifact is published.
func (s *Store) HasFrontend(repo, hash string) bool { return dirExists(s.FrontendDir(repo, hash)) }

// HasBackend reports whether the backend artifact is published.
func (s *Store) HasBackend(repo, hash string) bool { return dirExists(s.BackendDir(repo, hash)) }

// RemoveRepo deletes every published artifact and state directory for a
// repo (repo deletion).
func (s *Store) RemoveRepo(repo string) error {
	if err := os.RemoveAll(filepath.Join(s.artifactsDir, repo)); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.stateDir, repo))
}

// PublishFrontend moves srcDir into place as the frontend artifact.
// Idempotent: if the artifact already exists it is kept as-is unless
// overwrite is set (the --rebuild path).
func (s *Store) PublishFrontend(repo, hash, srcDir string, overwrite bool) error {
	return s.publish(s.FrontendDir(repo, hash), srcDir, overwrite)
}

// PublishBackend moves srcDir into place as the backend artifact.
func (s *Store) PublishBackend(repo, hash, srcDir string, overwrite bool) error {
	return s.publish(s.BackendDir(repo, hash), srcDir, overwrite)
}

func (s *Store) publish(dest, srcDir string, overwrite bool) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create artifact parent: %w", err)
	}
	if dirExists(dest) {
		if !overwrite {
			os.RemoveAll(srcDir)
			return nil
		}
		// Move the old artifact aside first so the window with no artifact
		// present is as small as possible, then discard it.
		trash, err := os.MkdirTemp(s.ensureTmp(), "trash-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(trash)
		if err := os.Rename(dest, filepath.Join(trash, "old")); err != nil {
			return fmt.Errorf("displace old artifact: %w", err)
		}
	}
	if err := os.Rename(srcDir, dest); err != nil {
		// A concurrent publisher of the same hash may have won the rename;
		// content-addressing makes their copy equivalent to ours.
		if dirExists(dest) {
			os.RemoveAll(srcDir)
			return nil
		}
		return fmt.Errorf("publish artifact: %w", err)
	}
	return nil
}

// NewScratchDir creates a staging directory under tmp/. The cleanup func is
// safe to call after a successful rename out of the scratch dir.
func (s *Store) NewScratchDir(purpose string) (string, func(), error) {
	dir, err := os.MkdirTemp(s.ensureTmp(), purpose+"-")
	if err != nil {
		return "", nil, fmt.Errorf("create scratch dir: %w", err)
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

// StateDirPath returns the mutable state directory for a backend artifact.
func (s *Store) StateDirPath(repo, beHash string) string {
	return filepath.Join(s.stateDir, repo, beHash)
}

// HasStateDir reports whether the state dir exists.
func (s *Store) HasStateDir(repo, beHash string) bool {
	return dirExists(s.StateDirPath(repo, beHash))
}

// InitFreshStateDir creates an empty state dir (the no-deployed-ancestor
// case). Idempotent.
func (s *Store) InitFreshStateDir(repo, beHash string) error {
	return os.MkdirAll(s.StateDirPath(repo, beHash), 0o755)
}

// ForkStateDir copies the src backend's state dir to dst: recursive copy
// into tmp/, then one atomic rename. No-op if dst already exists. The caller
// is responsible for quiescing whatever writes to src first.
func (s *Store) ForkStateDir(repo, srcBeHash, dstBeHash string) error {
	dst := s.StateDirPath(repo, dstBeHash)
	if dirExists(dst) {
		return nil
	}
	src := s.StateDirPath(repo, srcBeHash)
	if !dirExists(src) {
		return fmt.Errorf("fork state: source state dir for %s does not exist", srcBeHash)
	}
	staging, err := os.MkdirTemp(s.ensureTmp(), "fork-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	copied := filepath.Join(staging, "state")
	if err := copyDir(src, copied); err != nil {
		return fmt.Errorf("fork state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(copied, dst); err != nil {
		if dirExists(dst) {
			return nil
		}
		return fmt.Errorf("fork state: %w", err)
	}
	return nil
}

// SweepTmp removes staging entries older than maxAge — orphans from a crash.
func (s *Store) SweepTmp(maxAge time.Duration) error {
	entries, err := os.ReadDir(s.tmpDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.RemoveAll(filepath.Join(s.tmpDir, e.Name()))
		}
	}
	return nil
}

func (s *Store) ensureTmp() string {
	os.MkdirAll(s.tmpDir, 0o755)
	return s.tmpDir
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// copyDir recursively copies src to dst, preserving file modes. Regular
// files, directories, and symlinks only.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		default:
			return fmt.Errorf("unsupported file type %s in state dir: %s", info.Mode(), p)
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
