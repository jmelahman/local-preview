package api

import (
	"net/http"

	"github.com/jmelahman/local-preview/internal/retain"
	"github.com/jmelahman/local-preview/internal/usage"
)

// usageDirs locates the trees storage reporting walks.
func (d Deps) usageDirs() usage.Dirs {
	return usage.Dirs{
		Artifacts: d.Config.ArtifactsDir(),
		State:     d.Config.StateDir(),
		Logs:      d.Config.LogsDir(),
		Tmp:       d.Config.TmpDir(),
		DBPath:    d.DBPath,
	}
}

// handleStorage reports the instance's disk usage, by category and by repo.
func (d Deps) handleStorage(w http.ResponseWriter, r *http.Request) {
	rep, err := usage.Compute(d.Store, d.usageDirs(), d.durableTier())
	if err != nil {
		internalError(w, "compute storage usage", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// durableTier returns the durable artifact tier as a usage reporter, or nil
// when none is configured. The tier is owned by the store; storage reporting
// only needs its size accessor.
func (d Deps) durableTier() usage.DurableTier {
	t := d.Files.ArtifactTier()
	if t == nil {
		return nil
	}
	if dt, ok := t.(usage.DurableTier); ok {
		return dt
	}
	return nil
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
	if !decodeJSON(w, r, &p) {
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
	dirs := d.usageDirs()
	before := usage.Reclaimable(dirs)
	res, err := d.Sweeper.Sweep()
	if err != nil {
		internalError(w, "retention sweep", err)
		return
	}
	writeJSON(w, http.StatusOK, gcJSON{
		Result:     res,
		FreedBytes: max(0, before-usage.Reclaimable(dirs)),
	})
}
