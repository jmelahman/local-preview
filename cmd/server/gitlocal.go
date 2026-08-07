package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
)

// findWorktreeRoot walks up from dir to the first directory containing a
// .git entry — the equivalent of `git rev-parse --show-toplevel`.
func findWorktreeRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no .git found in %s or any parent", dir)
		}
		abs = parent
	}
}

// localGitDir returns the git directory for the working tree at root — the
// equivalent of `git rev-parse --git-dir`. For a linked worktree (.git is a
// file), it follows the gitdir pointer.
func localGitDir(root string) (string, error) {
	dotGit := filepath.Join(root, ".git")
	fi, err := os.Stat(dotGit)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return dotGit, nil
	}
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(strings.TrimPrefix(string(raw), "gitdir:"))
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	return target, nil
}

func openLocalRepo(dir string) (*git.Repository, error) {
	return git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{
		DetectDotGit:          true,
		EnableDotGitCommonDir: true,
	})
}

// localHeadSHA returns the commit sha of HEAD for the working tree
// containing dir — the equivalent of `git rev-parse HEAD`.
func localHeadSHA(dir string) (string, error) {
	repo, err := openLocalRepo(dir)
	if err != nil {
		return "", err
	}
	head, err := repo.Head()
	if err != nil {
		return "", err
	}
	return head.Hash().String(), nil
}

// localOriginURL returns the origin remote's URL for the working tree
// containing dir, or "" if there is no origin remote.
func localOriginURL(dir string) string {
	repo, err := openLocalRepo(dir)
	if err != nil {
		return ""
	}
	remote, err := repo.Remote("origin")
	if err != nil || len(remote.Config().URLs) == 0 {
		return ""
	}
	return remote.Config().URLs[0]
}
