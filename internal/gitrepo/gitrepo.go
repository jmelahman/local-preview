// Package gitrepo manages the orchestrator's mirror clones and exposes the
// git plumbing the pipeline needs: ref resolution, tree listing (for content
// addressing), file reads at a commit, ancestry walks, and tree extraction.
// It shells out to system git — behavior identical to what a developer sees —
// and has no database dependency.
package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// nameRE restricts repo names to a single lowercase RFC 1035 DNS label so
// Host-header parsing of <label>.<repo>.<domain> is unambiguous.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateName rejects repo names that can't serve as a DNS label.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid repo name %q: must be a lowercase DNS label (letters, digits, hyphens)", name)
	}
	return nil
}

// CheckGit verifies system git is available. Called once at server startup.
func CheckGit(ctx context.Context) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git binary not found in PATH: %w", err)
	}
	if _, err := runGit(ctx, "", "version"); err != nil {
		return err
	}
	return nil
}

// TreeEntry is one blob in a commit's recursive tree listing.
type TreeEntry struct {
	Mode string
	Type string
	OID  string
	Path string
}

// Manager owns the directory of mirror clones.
type Manager struct {
	dir string
}

// NewManager returns a Manager rooted at reposDir (created lazily).
func NewManager(reposDir string) *Manager {
	return &Manager{dir: reposDir}
}

// Repo is a handle to one mirror clone.
type Repo struct {
	Name string
	Path string
}

func (m *Manager) repoPath(name string) string {
	return filepath.Join(m.dir, name+".git")
}

// Open returns a handle for an already-registered repo.
func (m *Manager) Open(name string) Repo {
	return Repo{Name: name, Path: m.repoPath(name)}
}

// Add registers a repo by mirror-cloning source (a local path or clone URL).
// A mirror clone keeps refs/heads in sync on Fetch, unlike a plain --bare
// clone whose fetch refspec is unset.
func (m *Manager) Add(ctx context.Context, name, source string) (Repo, error) {
	if err := ValidateName(name); err != nil {
		return Repo{}, err
	}
	dest := m.repoPath(name)
	if _, err := os.Stat(dest); err == nil {
		return Repo{}, fmt.Errorf("repo %q already exists", name)
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return Repo{}, fmt.Errorf("create repos dir: %w", err)
	}
	// Absolutize local paths so the clone keeps working regardless of the
	// server's cwd.
	if st, err := os.Stat(source); err == nil && st.IsDir() {
		abs, err := filepath.Abs(source)
		if err != nil {
			return Repo{}, fmt.Errorf("resolve source path: %w", err)
		}
		source = abs
	}
	if _, err := runGit(ctx, "", "clone", "--mirror", "--quiet", source, dest); err != nil {
		os.RemoveAll(dest)
		return Repo{}, err
	}
	return Repo{Name: name, Path: dest}, nil
}

// Remove deletes the mirror clone for name. A missing clone is not an
// error.
func (m *Manager) Remove(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return os.RemoveAll(m.repoPath(name))
}

// Fetch updates all refs from origin, pruning deleted ones.
func (r Repo) Fetch(ctx context.Context) error {
	_, err := runGit(ctx, r.Path, "fetch", "--quiet", "--prune", "origin")
	return err
}

// ResolveRef resolves a branch, tag, or (abbreviated) sha to a full commit
// sha, fetching once and retrying if the ref isn't known locally yet.
func (r Repo) ResolveRef(ctx context.Context, ref string) (string, error) {
	sha, err := r.revParse(ctx, ref)
	if err == nil {
		return sha, nil
	}
	if ferr := r.Fetch(ctx); ferr != nil {
		return "", fmt.Errorf("resolve %q: %w (fetch failed: %v)", ref, err, ferr)
	}
	sha, err = r.revParse(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", ref, err)
	}
	return sha, nil
}

func (r Repo) revParse(ctx context.Context, ref string) (string, error) {
	out, err := runGit(ctx, r.Path, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CommitMeta is display metadata for one commit.
type CommitMeta struct {
	AuthorName  string
	AuthorEmail string
}

// CommitMeta returns the author of sha.
func (r Repo) CommitMeta(ctx context.Context, sha string) (CommitMeta, error) {
	out, err := runGit(ctx, r.Path, "show", "-s", "--format=%an%x00%ae", sha)
	if err != nil {
		return CommitMeta{}, err
	}
	name, email, ok := strings.Cut(strings.TrimRight(string(out), "\n"), "\x00")
	if !ok {
		return CommitMeta{}, fmt.Errorf("unexpected commit format for %s: %q", sha, out)
	}
	return CommitMeta{AuthorName: name, AuthorEmail: email}, nil
}

// IsBranch reports whether name is a branch head in the mirror.
func (r Repo) IsBranch(ctx context.Context, name string) bool {
	_, err := runGit(ctx, r.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// BranchesPointingAt returns the branches whose tip is sha, in refname
// order. Commits that are no longer any branch's tip return none.
func (r Repo) BranchesPointingAt(ctx context.Context, sha string) ([]string, error) {
	out, err := runGit(ctx, r.Path,
		"for-each-ref", "--points-at", sha, "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var branches []string
	for line := range strings.Lines(string(out)) {
		if b := strings.TrimSpace(line); b != "" {
			branches = append(branches, b)
		}
	}
	return branches, nil
}

// BranchTip is one branch head in the mirror.
type BranchTip struct {
	Branch string
	SHA    string
}

// BranchTips returns every branch head in the mirror, in refname order.
func (r Repo) BranchTips(ctx context.Context) ([]BranchTip, error) {
	out, err := runGit(ctx, r.Path,
		"for-each-ref", "--format=%(refname:short)%00%(objectname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var tips []BranchTip
	for line := range strings.Lines(string(out)) {
		branch, sha, ok := strings.Cut(strings.TrimSpace(line), "\x00")
		if !ok {
			continue
		}
		tips = append(tips, BranchTip{Branch: branch, SHA: sha})
	}
	return tips, nil
}

// UnreachableSHAs returns the subset of candidates no longer reachable from
// any of tips — commits that have fallen out of the repo's branch history
// because an unmerged branch was deleted or a tip was force-pushed past them.
// Every argument is a full or abbreviated object name; tips are the surviving
// branch heads (see BranchTips), gathered after a fetch --prune.
//
// One walk does it: `git rev-list <candidates> --not <tips>` enumerates every
// commit reachable from a candidate but from no tip, so a candidate appearing
// in the output is itself unreachable. --ignore-missing drops candidates the
// mirror has already gc'd rather than erroring; those are left for a later
// pass, keeping the result conservative. With no surviving tips every present
// candidate is unreachable. Output order follows git's; each match is
// returned once.
func (r Repo) UnreachableSHAs(ctx context.Context, candidates, tips []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	args := append([]string{"rev-list", "--ignore-missing"}, candidates...)
	if len(tips) > 0 {
		args = append(args, "--not")
		args = append(args, tips...)
	}
	out, err := runGit(ctx, r.Path, args...)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		want[c] = true
	}
	var unreachable []string
	for line := range strings.Lines(string(out)) {
		sha := strings.TrimSpace(line)
		if want[sha] {
			delete(want, sha) // dedupe; each candidate reported at most once
			unreachable = append(unreachable, sha)
		}
	}
	return unreachable, nil
}

// ReadFile returns the content of path at sha.
func (r Repo) ReadFile(ctx context.Context, sha, path string) ([]byte, error) {
	return runGit(ctx, r.Path, "show", sha+":"+path)
}

// LsTree lists all blobs at sha, optionally restricted to a subtree.
// Submodule (commit) entries are skipped — submodules are out of scope.
// Entries come back in git's sorted order, which content hashing relies on.
func (r Repo) LsTree(ctx context.Context, sha, sub string) ([]TreeEntry, error) {
	args := []string{"ls-tree", "-r", "-z", "--full-tree", sha}
	if sub != "" && sub != "." {
		args = append(args, "--", sub)
	}
	out, err := runGit(ctx, r.Path, args...)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		// "<mode> <type> <oid>\t<path>"
		head, path, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("unexpected ls-tree record %q", rec)
		}
		fields := strings.Fields(string(head))
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected ls-tree record %q", rec)
		}
		if fields[1] != "blob" {
			continue
		}
		entries = append(entries, TreeEntry{
			Mode: fields[0],
			Type: fields[1],
			OID:  fields[2],
			Path: string(path),
		})
	}
	return entries, nil
}

// FirstParentAncestry returns up to limit ancestors of sha (excluding sha
// itself), nearest first, following only first parents. Merge commits
// therefore walk the branch that received the merge.
func (r Repo) FirstParentAncestry(ctx context.Context, sha string, limit int) ([]string, error) {
	out, err := runGit(ctx, r.Path,
		"rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", limit+1), sha)
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(string(out))
	if len(lines) > 0 && lines[0] == sha {
		lines = lines[1:]
	}
	return lines, nil
}

// Archive extracts the full tree at sha into dest via git archive | tar.
// Stateless by design: it reads only the object database and needs no
// cleanup bookkeeping, so concurrent extractions are always safe.
func (r Repo) Archive(ctx context.Context, sha, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("create archive dest: %w", err)
	}
	arch := exec.CommandContext(ctx, "git", "archive", "--format=tar", sha)
	arch.Dir = r.Path
	var archErr bytes.Buffer
	arch.Stderr = &archErr

	pipe, err := arch.StdoutPipe()
	if err != nil {
		return err
	}
	untar := exec.CommandContext(ctx, "tar", "-x", "-C", dest)
	untar.Stdin = pipe
	var tarErr bytes.Buffer
	untar.Stderr = &tarErr

	if err := arch.Start(); err != nil {
		return fmt.Errorf("git archive: %w", err)
	}
	if err := untar.Start(); err != nil {
		arch.Process.Kill()
		arch.Wait()
		return fmt.Errorf("tar -x: %w", err)
	}
	archWaitErr := arch.Wait()
	tarWaitErr := untar.Wait()
	if archWaitErr != nil {
		return fmt.Errorf("git archive %s: %w: %s", sha, archWaitErr, strings.TrimSpace(archErr.String()))
	}
	if tarWaitErr != nil {
		return fmt.Errorf("tar -x: %w: %s", tarWaitErr, strings.TrimSpace(tarErr.String()))
	}
	return nil
}

// runGit executes git with args in dir (empty = inherit cwd), returning
// stdout and folding stderr into any error.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
