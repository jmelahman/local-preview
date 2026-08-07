package api

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/retain"
)

// repoUsageJSON is one repo's slice of the storage report.
type repoUsageJSON struct {
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

// storageJSON is the GET /api/storage response: how much disk the instance
// uses, by category and by repo.
type storageJSON struct {
	TotalBytes     int64           `json:"total_bytes"`
	ArtifactsBytes int64           `json:"artifacts_bytes"`
	StateBytes     int64           `json:"state_bytes"`
	LogsBytes      int64           `json:"logs_bytes"`
	MirrorBytes    int64           `json:"mirror_bytes"`
	TmpBytes       int64           `json:"tmp_bytes"`
	DBBytes        int64           `json:"db_bytes"`
	Repos          []repoUsageJSON `json:"repos"`
}

// handleStorage reports the instance's disk usage. Sizes are computed by
// walking the data dir on every call — previews are local-first and the
// tree is modest, so live numbers beat a stale cache.
func (d Deps) handleStorage(w http.ResponseWriter, r *http.Request) {
	repos, err := d.Store.ListRepos()
	if err != nil {
		internalError(w, "list repos", err)
		return
	}
	deploys, err := d.Store.ListDeploys(db.DeployFilter{})
	if err != nil {
		internalError(w, "list deploys", err)
		return
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

	resp := storageJSON{
		TmpBytes: dirSize(d.Config.TmpDir()),
		DBBytes:  dbSize(d.DBPath),
		Repos:    []repoUsageJSON{},
	}
	for _, repo := range repos {
		u := repoUsageJSON{
			Repo:           repo.Name,
			ArtifactsBytes: dirSize(filepath.Join(d.Config.ArtifactsDir(), repo.Name)),
			StateBytes:     dirSize(filepath.Join(d.Config.StateDir(), repo.Name)),
			LogsBytes:      dirSize(filepath.Join(d.Config.LogsDir(), repo.Name)),
			MirrorBytes:    dirSize(repo.BarePath),
			Deploys:        active[repo.Name],
			EvictedDeploys: evicted[repo.Name],
		}
		u.TotalBytes = u.ArtifactsBytes + u.StateBytes + u.LogsBytes + u.MirrorBytes
		resp.ArtifactsBytes += u.ArtifactsBytes
		resp.StateBytes += u.StateBytes
		resp.LogsBytes += u.LogsBytes
		resp.MirrorBytes += u.MirrorBytes
		resp.Repos = append(resp.Repos, u)
	}
	resp.TotalBytes = resp.ArtifactsBytes + resp.StateBytes + resp.LogsBytes +
		resp.MirrorBytes + resp.TmpBytes + resp.DBBytes
	writeJSON(w, http.StatusOK, resp)
}

func (d Deps) handleGetRetention(w http.ResponseWriter, r *http.Request) {
	policy, err := retain.LoadPolicy(d.Store)
	if err != nil {
		internalError(w, "load retention policy", err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

// handlePutRetention replaces the retention policy. The new policy takes
// effect on the next sweep — hourly, or immediately via POST /api/gc — so
// tightening limits never surprise-evicts on save.
func (d Deps) handlePutRetention(w http.ResponseWriter, r *http.Request) {
	var p retain.Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := p.Validate(); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := retain.SavePolicy(d.Store, p); err != nil {
		internalError(w, "save retention policy", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// gcJSON is the POST /api/gc response: what the sweep evicted and how much
// disk it gave back.
type gcJSON struct {
	retain.Result
	FreedBytes int64 `json:"freed_bytes"`
}

// handleRunGC runs one retention sweep immediately. With retention disabled
// it still collects stale tmp/ staging leftovers.
func (d Deps) handleRunGC(w http.ResponseWriter, r *http.Request) {
	before := d.dataSize()
	res, err := d.Sweeper.Sweep()
	if err != nil {
		internalError(w, "retention sweep", err)
		return
	}
	writeJSON(w, http.StatusOK, gcJSON{
		Result:     res,
		FreedBytes: max(0, before-d.dataSize()),
	})
}

// dataSize totals the categories a sweep can shrink.
func (d Deps) dataSize() int64 {
	return dirSize(d.Config.ArtifactsDir()) + dirSize(d.Config.StateDir()) +
		dirSize(d.Config.LogsDir()) + dirSize(d.Config.TmpDir())
}

// dirSize walks root summing regular-file sizes. Unreadable entries are
// skipped: usage reporting must never fail on a permission oddity.
func dirSize(root string) int64 {
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
// --in-memory instances.
func dbSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			total += st.Size()
		}
	}
	return total
}
