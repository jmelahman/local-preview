// Package usage measures the instance's disk footprint: how many bytes the
// artifacts, backend state, logs, mirror clones, staging area, and database
// occupy, totalled and broken down per repo. Sizes are computed by walking
// the data dir on every call — previews are local-first and the tree is
// modest, so live numbers beat a stale cache.
package usage

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jmelahman/local-preview/internal/db"
)

// Dirs locates the trees a report covers. DBPath is the SQLite location
// actually opened (":memory:" for an ephemeral instance), not a default —
// reporting must size the real database, never a stale file from a
// previous on-disk run.
type Dirs struct {
	Artifacts string
	State     string
	Logs      string
	Tmp       string
	DBPath    string
}

// RepoUsage is one repo's slice of the report.
type RepoUsage struct {
	Repo string `json:"repo"`
	// ArtifactsBytes covers published fe/be/dl artifacts; StateBytes the
	// mutable backend state dirs; LogsBytes build and run logs; MirrorBytes
	// the bare git clone.
	ArtifactsBytes int64 `json:"artifacts_bytes"`
	StateBytes     int64 `json:"state_bytes"`
	LogsBytes      int64 `json:"logs_bytes"`
	MirrorBytes    int64 `json:"mirror_bytes"`
	TotalBytes     int64 `json:"total_bytes"`
	// Deploys counts the repo's non-evicted deploys; EvictedDeploys the
	// evicted ones (rows kept as history, artifacts reclaimed).
	Deploys        int `json:"deploys"`
	EvictedDeploys int `json:"evicted_deploys"`
}

// Report is the instance's disk usage, by category and by repo.
type Report struct {
	TotalBytes     int64       `json:"total_bytes"`
	ArtifactsBytes int64       `json:"artifacts_bytes"`
	StateBytes     int64       `json:"state_bytes"`
	LogsBytes      int64       `json:"logs_bytes"`
	MirrorBytes    int64       `json:"mirror_bytes"`
	TmpBytes       int64       `json:"tmp_bytes"`
	DBBytes        int64       `json:"db_bytes"`
	Repos          []RepoUsage `json:"repos"`
}

// Compute walks dirs and totals the instance's usage.
func Compute(database *db.Store, dirs Dirs) (Report, error) {
	repos, err := database.ListRepos()
	if err != nil {
		return Report{}, err
	}
	deploys, err := database.ListDeploys(db.DeployFilter{})
	if err != nil {
		return Report{}, err
	}
	active := map[string]int{}
	evicted := map[string]int{}
	for _, row := range deploys {
		if row.Status == db.DeployEvicted {
			evicted[row.RepoName]++
		} else {
			active[row.RepoName]++
		}
	}

	rep := Report{
		TmpBytes: DirSize(dirs.Tmp),
		DBBytes:  dbSize(dirs.DBPath),
		Repos:    []RepoUsage{},
	}
	for _, repo := range repos {
		u := RepoUsage{
			Repo:           repo.Name,
			ArtifactsBytes: DirSize(filepath.Join(dirs.Artifacts, repo.Name)),
			StateBytes:     DirSize(filepath.Join(dirs.State, repo.Name)),
			LogsBytes:      DirSize(filepath.Join(dirs.Logs, repo.Name)),
			MirrorBytes:    DirSize(repo.BarePath),
			Deploys:        active[repo.Name],
			EvictedDeploys: evicted[repo.Name],
		}
		u.TotalBytes = u.ArtifactsBytes + u.StateBytes + u.LogsBytes + u.MirrorBytes
		rep.ArtifactsBytes += u.ArtifactsBytes
		rep.StateBytes += u.StateBytes
		rep.LogsBytes += u.LogsBytes
		rep.MirrorBytes += u.MirrorBytes
		rep.Repos = append(rep.Repos, u)
	}
	rep.TotalBytes = rep.ArtifactsBytes + rep.StateBytes + rep.LogsBytes +
		rep.MirrorBytes + rep.TmpBytes + rep.DBBytes
	return rep, nil
}

// Reclaimable totals only the categories a retention sweep can shrink —
// the baseline for reporting how much a sweep gave back.
func Reclaimable(dirs Dirs) int64 {
	return DirSize(dirs.Artifacts) + DirSize(dirs.State) +
		DirSize(dirs.Logs) + DirSize(dirs.Tmp)
}

// DirSize walks root summing regular-file sizes. Unreadable entries are
// skipped: usage reporting must never fail on a permission oddity.
func DirSize(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fs.SkipDir
		}
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// dbSize sizes the SQLite database including its WAL sidecars; 0 for
// in-memory instances.
func dbSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			total += st.Size()
		}
	}
	return total
}
