// Package supervise owns preview process lifecycles: one process per
// distinct artifact per repo (backends, and process-mode frontends),
// started on demand, health-checked, and stopped as a unit. A process runs
// either directly on the host (killed as a process group) or, with a
// manifest run_image, inside a labeled container. It also provisions
// backend state directories via lineage forking — the state for a new
// backend hash is copied from the nearest deployed ancestor's backend,
// quiesced first.
//
// The in-memory process table is authoritative while the orchestrator runs;
// process_records rows (host processes) and container labels (containered
// processes) exist only so an unclean exit can be reclaimed at next
// startup.
package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/dockerapi"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/manifest"
	"github.com/jmelahman/local-preview/internal/store"
)

// AncestryLimit caps how far the lineage walk searches for a fork source
// before falling back to a fresh state dir.
const AncestryLimit = 500

// stopGrace is how long a SIGTERM'd process gets before SIGKILL.
const stopGrace = 5 * time.Second

// Side distinguishes the two supervised process kinds.
type Side string

// Sides.
const (
	SideBackend  Side = "be"
	SideFrontend Side = "fe"
)

// Key identifies a supervised process: one per artifact per repo. Peer is
// the deploy's backend hash for process-mode frontends — a {backend_url}
// env binding makes a frontend instance specific to one backend, while the
// built artifact stays shared per fe_hash.
type Key struct {
	RepoID int64
	Side   Side
	Hash   string
	Peer   string
}

// BackendKey identifies a backend process.
func BackendKey(repoID int64, beHash string) Key {
	return Key{RepoID: repoID, Side: SideBackend, Hash: beHash}
}

// FrontendKey identifies a process-mode frontend paired with the deploy's
// backend.
func FrontendKey(repoID int64, feHash, beHash string) Key {
	return Key{RepoID: repoID, Side: SideFrontend, Hash: feHash, Peer: beHash}
}

// Manager supervises preview processes.
type Manager struct {
	db      *db.Store
	files   *store.Store
	logsDir string

	healthInterval time.Duration
	reapInterval   time.Duration
	maxWarm        int // LRU cap on running processes; 0 = unlimited

	mu    sync.Mutex
	procs map[Key]*process
	locks map[Key]*sync.Mutex

	dockerMu     sync.Mutex
	dockerProbed bool
	dockerCli    *dockerapi.Client
	dockerErr    error
}

// New returns a Manager. logsDir is the root for per-process run logs.
func New(database *db.Store, files *store.Store, logsDir string) *Manager {
	return &Manager{
		db:             database,
		files:          files,
		logsDir:        logsDir,
		healthInterval: 200 * time.Millisecond,
		reapInterval:   30 * time.Second,
		procs:          make(map[Key]*process),
		locks:          make(map[Key]*sync.Mutex),
	}
}

// SetMaxWarm caps concurrently running processes: beyond it the
// least-recently-used are stopped. Non-positive disables the cap. Call
// before StartReaper.
func (m *Manager) SetMaxWarm(n int) {
	m.maxWarm = max(n, 0)
}

// StartReaper launches the loop enforcing idle timeouts and the warm cap.
func (m *Manager) StartReaper(ctx context.Context) {
	go func() {
		tick := time.NewTicker(m.reapInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				m.reap()
			}
		}
	}()
}

// docker dials the daemon lazily, once — mirrors build.autoRunner.
func (m *Manager) docker(ctx context.Context) (*dockerapi.Client, error) {
	m.dockerMu.Lock()
	defer m.dockerMu.Unlock()
	if !m.dockerProbed {
		m.dockerProbed = true
		m.dockerCli, m.dockerErr = dockerapi.Connect(ctx)
	}
	return m.dockerCli, m.dockerErr
}

// process is one supervised child — a host child process (cmd set) or a
// container (containerID set). ready is closed once healthy; failed is
// closed if the start attempt errors (err is set first). done is closed
// when the process exits, however that happens.
type process struct {
	repoName    string
	port        int
	cmd         *exec.Cmd
	containerID string

	lastTouch time.Time     // guarded by Manager.mu; recency for the reaper
	idleAfter time.Duration // idle_timeout from the run config

	ready  chan struct{}
	failed chan struct{}
	done   chan struct{}
	err    error

	intentional bool // set (under the key lock) before a deliberate stop
}

func (m *Manager) keyLock(k Key) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[k]
	if !ok {
		l = &sync.Mutex{}
		m.locks[k] = l
	}
	return l
}

// EnsureRunning returns the port of a healthy process for the artifact
// key, starting one if needed. The start itself is never cancelled by
// ctx — a caller that gives up (e.g. a proxy request with a short deadline)
// leaves the start running for the next request to pick up.
func (m *Manager) EnsureRunning(ctx context.Context, k Key, repoName string) (int, error) {
	for range 2 {
		m.mu.Lock()
		p := m.procs[k]
		if p == nil {
			p = &process{
				repoName: repoName,
				ready:    make(chan struct{}),
				failed:   make(chan struct{}),
				done:     make(chan struct{}),
			}
			m.procs[k] = p
			go m.start(k, p)
		}
		p.lastTouch = time.Now()
		// SSR frontends call their backend directly over the deploy
		// network, invisible to the proxy — the pair's recency travels
		// with the frontend's touches.
		if k.Side == SideFrontend && k.Peer != "" {
			if bp := m.procs[BackendKey(k.RepoID, k.Peer)]; bp != nil {
				bp.lastTouch = p.lastTouch
			}
		}
		m.mu.Unlock()

		select {
		case <-p.ready:
			select {
			case <-p.done:
				// Died after becoming healthy; clear and retry once.
				m.forget(k, p)
				continue
			default:
				return p.port, nil
			}
		case <-p.failed:
			m.forget(k, p)
			return 0, p.err
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, fmt.Errorf("%s %s: process exited immediately after start", k.Side, k.Hash[:12])
}

// forget removes p from the table if it's still the tracked entry.
func (m *Manager) forget(k Key, p *process) {
	m.mu.Lock()
	if m.procs[k] == p {
		delete(m.procs, k)
	}
	m.mu.Unlock()
}

// runSpec is the side-independent run contract loaded from the artifact's
// stored run_config.
type runSpec struct {
	argv         []string
	runImage     string
	env          map[string]string
	healthPath   string
	startTimeout time.Duration
	idleTimeout  time.Duration
	dir          string // artifact dir: host cwd, or container bind + workdir
	stateDir     string // backend only
	networks     []string
}

// Stored run_config shapes: the manifest section plus the top-level
// networks list, marshaled together at build time.
type backendRunConfig struct {
	manifest.Backend
	Networks []string `json:"networks,omitempty"`
}

type frontendRunConfig struct {
	manifest.Frontend
	Networks []string `json:"networks,omitempty"`
}

// loadRunSpec resolves the artifact's run contract for either side.
func (m *Manager) loadRunSpec(k Key, repoName string) (runSpec, error) {
	if k.Side == SideFrontend {
		art, err := m.db.GetFrontendArtifact(k.RepoID, k.Hash)
		if err != nil {
			return runSpec{}, fmt.Errorf("frontend artifact %s not provisioned: %w", k.Hash[:12], err)
		}
		var cfg frontendRunConfig
		if err := json.Unmarshal([]byte(art.RunConfig), &cfg); err != nil {
			return runSpec{}, fmt.Errorf("parse run config: %w", err)
		}
		return runSpec{
			argv:         cfg.Run,
			runImage:     cfg.RunImage,
			env:          cfg.Env,
			healthPath:   cfg.HealthPath,
			startTimeout: time.Duration(cfg.StartTimeout),
			idleTimeout:  idleOrDefault(time.Duration(cfg.IdleTimeout)),
			dir:          m.files.FrontendDir(repoName, k.Hash),
			networks:     cfg.Networks,
		}, nil
	}
	art, err := m.db.GetBackendArtifact(k.RepoID, k.Hash)
	if err != nil {
		return runSpec{}, fmt.Errorf("backend artifact %s not provisioned: %w", k.Hash[:12], err)
	}
	var cfg backendRunConfig
	if err := json.Unmarshal([]byte(art.RunConfig), &cfg); err != nil {
		return runSpec{}, fmt.Errorf("parse run config: %w", err)
	}
	return runSpec{
		argv:         cfg.Run,
		runImage:     cfg.RunImage,
		env:          cfg.Env,
		healthPath:   cfg.HealthPath,
		startTimeout: time.Duration(cfg.StartTimeout),
		idleTimeout:  idleOrDefault(time.Duration(cfg.IdleTimeout)),
		dir:          m.files.BackendDir(repoName, k.Hash),
		stateDir:     art.StateDir,
		networks:     cfg.Networks,
	}, nil
}

// idleOrDefault covers run configs stored before idle enforcement existed.
func idleOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return manifest.DefaultIdleTimeout
	}
	return d
}

// start runs the full start sequence: load run config, probe a port, exec
// (host or container), health-poll. It owns its own timeout and runs
// detached from any request context. Exactly one start goroutine exists
// per tracked process.
func (m *Manager) start(k Key, p *process) {
	lock := m.keyLock(k)
	lock.Lock()
	defer lock.Unlock()

	fail := func(event string, err error) {
		p.err = err
		m.db.AddProcessEvent(k.RepoID, k.Hash, event, err.Error())
		close(p.failed)
		m.forget(k, p)
	}

	spec, err := m.loadRunSpec(k, p.repoName)
	if err != nil {
		fail("start_attempt", err)
		return
	}
	p.idleAfter = spec.idleTimeout
	if _, err := os.Stat(spec.dir); err != nil {
		fail("start_attempt", fmt.Errorf("%s artifact files missing (evicted?): %w", k.Side, err))
		return
	}

	// A frontend whose env references {backend_url} needs its peer backend
	// up first — the URL carries the backend's port.
	backendURL := ""
	if k.Side == SideFrontend && envReferences(spec.env, "{backend_url}") {
		if k.Peer == "" {
			fail("start_attempt", fmt.Errorf("{backend_url} referenced but deploy has no backend"))
			return
		}
		bePort, err := m.EnsureRunning(context.Background(), BackendKey(k.RepoID, k.Peer), p.repoName)
		if err != nil {
			fail("start_attempt", fmt.Errorf("start backend for {backend_url}: %w", err))
			return
		}
		if spec.runImage != "" {
			// Both sides containered (manifest-validated): the deploy
			// network resolves the backend by alias.
			backendURL = fmt.Sprintf("http://backend:%d", bePort)
		} else {
			backendURL = fmt.Sprintf("http://127.0.0.1:%d", bePort)
		}
	}

	port, err := probeFreePort()
	if err != nil {
		fail("start_attempt", err)
		return
	}
	p.port = port

	logFile, err := m.openRunLog(p.repoName, string(k.Side)+"-"+k.Hash)
	if err != nil {
		fail("start_attempt", err)
		return
	}

	argv := templateArgv(spec.argv, port, spec.stateDir)
	envPairs := expandEnv(spec.env, p.repoName, k.Hash, port, spec.stateDir, backendURL)

	if spec.runImage != "" {
		if err := m.startContainer(k, p, spec, argv, envPairs, port, logFile); err != nil {
			logFile.Close()
			fail("start_attempt", err)
			return
		}
	} else {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = spec.dir
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.Env = append(os.Environ(), envPairs...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			logFile.Close()
			fail("start_attempt", fmt.Errorf("start %s: %w", k.Side, err))
			return
		}
		p.cmd = cmd
		m.db.AddProcessEvent(k.RepoID, k.Hash, "start_attempt",
			fmt.Sprintf("pid %d port %d", cmd.Process.Pid, port))
		m.db.UpsertProcessRecord(db.ProcessRecord{
			RepoID: k.RepoID, BeHash: k.Hash,
			PID: cmd.Process.Pid, PGID: cmd.Process.Pid, Port: port,
		})

		// Reaper: closes done when the child exits and clears bookkeeping.
		go func() {
			waitErr := cmd.Wait()
			logFile.Close()
			close(p.done)
			m.forget(k, p)
			m.db.DeleteProcessRecord(k.RepoID, k.Hash)
			if !p.intentional {
				detail := "exit ok"
				if waitErr != nil {
					detail = waitErr.Error()
				}
				m.db.AddProcessEvent(k.RepoID, k.Hash, "exited", detail)
			}
		}()
	}

	if err := m.awaitHealthy(p, spec.healthPath, spec.startTimeout, port); err != nil {
		// Only kill what's still alive: force-removing an already-exited
		// container would cut its log stream before the crash output
		// (reaped with a drain grace) lands in the run log.
		if !isExited(p) {
			m.killProcess(p)
		}
		<-p.done
		event := "health_timeout"
		if isExited(p) {
			event = "exited"
		}
		fail(event, err)
		return
	}
	m.db.AddProcessEvent(k.RepoID, k.Hash, "healthy", fmt.Sprintf("port %d", port))
	close(p.ready)
	if m.maxWarm > 0 {
		// Enforce the warm cap promptly rather than waiting a reap tick.
		go m.reap()
	}
}

// isReady reports whether p passed its health check.
func isReady(p *process) bool {
	select {
	case <-p.ready:
		return true
	default:
		return false
	}
}

// reap enforces idle timeouts and the LRU warm cap. Backends inherit their
// running frontend's recency and are never reaped while that frontend
// runs: the frontend holds the backend's port in its env, so a backend
// restart (new port) would strand it.
func (m *Manager) reap() {
	type cand struct {
		k     Key
		touch time.Time
		idle  time.Duration
	}
	now := time.Now()
	m.mu.Lock()
	var cands []cand
	pairedTouch := make(map[Key]time.Time) // backend key → its live frontend's touch
	for k, p := range m.procs {
		if !isReady(p) || isExited(p) {
			continue
		}
		cands = append(cands, cand{k: k, touch: p.lastTouch, idle: p.idleAfter})
		if k.Side == SideFrontend && k.Peer != "" {
			bk := BackendKey(k.RepoID, k.Peer)
			if p.lastTouch.After(pairedTouch[bk]) {
				pairedTouch[bk] = p.lastTouch
			}
		}
	}
	m.mu.Unlock()

	alive := cands[:0]
	for _, c := range cands {
		if _, paired := pairedTouch[c.k]; paired {
			alive = append(alive, c) // counted toward the cap, never stopped
			continue
		}
		if c.idle > 0 && now.Sub(c.touch) > c.idle {
			m.Stop(c.k, fmt.Sprintf("idle %s", now.Sub(c.touch).Round(time.Second)))
			continue
		}
		alive = append(alive, c)
	}
	if m.maxWarm <= 0 || len(alive) <= m.maxWarm {
		return
	}
	sort.Slice(alive, func(i, j int) bool {
		if alive[i].touch.Equal(alive[j].touch) {
			// Stop a pair's frontend before its backend.
			return alive[i].k.Side == SideFrontend && alive[j].k.Side == SideBackend
		}
		return alive[i].touch.Before(alive[j].touch)
	})
	excess := len(alive) - m.maxWarm
	for _, c := range alive {
		if excess == 0 {
			return
		}
		if _, paired := pairedTouch[c.k]; paired {
			continue
		}
		m.Stop(c.k, fmt.Sprintf("least-recently-used beyond max-warm %d", m.maxWarm))
		excess--
	}
}

// expandEnv renders manifest env declarations into KEY=value pairs,
// substituting the run-time placeholders. {hash} is the side's own 12-char
// artifact hash so isolation follows artifact identity like state dirs do.
func expandEnv(env map[string]string, repo, hash string, port int, stateDir, backendURL string) []string {
	if len(env) == 0 {
		return nil
	}
	r := strings.NewReplacer(
		"{port}", strconv.Itoa(port),
		"{state_dir}", stateDir,
		"{repo}", repo,
		"{hash}", hash[:12],
		"{backend_url}", backendURL,
	)
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+r.Replace(v))
	}
	sort.Strings(out)
	return out
}

func envReferences(env map[string]string, placeholder string) bool {
	for _, v := range env {
		if strings.Contains(v, placeholder) {
			return true
		}
	}
	return false
}

func isExited(p *process) bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// awaitHealthy polls the health path until 200, the process exits, or the
// start timeout elapses.
func (m *Manager) awaitHealthy(p *process, healthPath string, timeout time.Duration, port int) error {
	if timeout <= 0 {
		timeout = manifest.DefaultStartTimeout
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(m.healthInterval)
	defer tick.Stop()
	client := &http.Client{Timeout: time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, healthPath)
	for {
		select {
		case <-p.done:
			return fmt.Errorf("process exited during startup (see run log)")
		case <-deadline:
			return fmt.Errorf("process did not become healthy within %s", timeout)
		case <-tick.C:
			resp, err := client.Get(url)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
	}
}

// Stop gracefully stops the process for key, if running. Reason lands in
// the process_events trail.
func (m *Manager) Stop(k Key, reason string) {
	lock := m.keyLock(k)
	lock.Lock()
	defer lock.Unlock()
	m.stopLocked(k, reason)
}

// stopLocked stops the tracked process while the caller holds the key lock.
func (m *Manager) stopLocked(k Key, reason string) {
	m.mu.Lock()
	p := m.procs[k]
	m.mu.Unlock()
	if p == nil {
		return
	}
	switch {
	case p.containerID != "":
		p.intentional = true
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cli, err := m.docker(ctx); err == nil {
			// The daemon owns the SIGTERM→SIGKILL grace window.
			cli.StopContainer(ctx, p.containerID, int(stopGrace/time.Second)) //nolint:errcheck
		}
		select {
		case <-p.done:
		case <-time.After(stopGrace + 10*time.Second):
			m.killProcess(p)
			<-p.done
		}
	case p.cmd != nil && p.cmd.Process != nil:
		p.intentional = true
		pid := p.cmd.Process.Pid
		syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(stopGrace):
			m.killProcess(p)
			<-p.done
		}
	default:
		return
	}
	m.forget(k, p)
	m.db.AddProcessEvent(k.RepoID, k.Hash, "idle_stop", reason)
}

// killProcess force-terminates either process kind.
func (m *Manager) killProcess(p *process) {
	if p.containerID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cli, err := m.docker(ctx); err == nil {
			cli.RemoveContainer(ctx, p.containerID, true) //nolint:errcheck
		}
		return
	}
	if p.cmd != nil && p.cmd.Process != nil {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
}

// StopAll gracefully stops every supervised process (orchestrator
// shutdown). Children never outlive the orchestrator by design.
func (m *Manager) StopAll() {
	m.mu.Lock()
	keys := make([]Key, 0, len(m.procs))
	for k := range m.procs {
		keys = append(keys, k)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Go(func() { m.Stop(k, "shutdown") })
	}
	wg.Wait()
}

// StopRepo gracefully stops every supervised process belonging to one repo
// (repo deletion).
func (m *Manager) StopRepo(repoID int64, reason string) {
	m.mu.Lock()
	keys := make([]Key, 0, len(m.procs))
	for k := range m.procs {
		if k.RepoID == repoID {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Go(func() { m.Stop(k, reason) })
	}
	wg.Wait()
}

// Status reports the runtime state of an artifact's process for API views:
// "running", "starting", or "idle" (not running; a start is one request
// away).
func (m *Manager) Status(k Key) string {
	m.mu.Lock()
	p := m.procs[k]
	m.mu.Unlock()
	if p == nil {
		return "idle"
	}
	select {
	case <-p.ready:
		return "running"
	default:
		return "starting"
	}
}

// ReclaimOrphans handles leftovers from an unclean exit. Host processes:
// if a process_records pid still leads a matching process group, the group
// is killed (PID reuse makes certain identification impossible; a health
// check always precedes routing, so a stale guess here can never misroute
// traffic). Containers: everything carrying the managed label is removed —
// labels are the container-side crash record. Docker being unreachable
// must never block startup; the sweep is skipped silently then.
func (m *Manager) ReclaimOrphans() {
	records, err := m.db.ListProcessRecords()
	if err == nil {
		for _, r := range records {
			if pgid, err := syscall.Getpgid(r.PID); err == nil && pgid == r.PGID {
				syscall.Kill(-r.PGID, syscall.SIGTERM)
				time.Sleep(200 * time.Millisecond)
				syscall.Kill(-r.PGID, syscall.SIGKILL)
				m.db.AddProcessEvent(r.RepoID, r.BeHash, "exited", "reclaimed orphan after unclean shutdown")
			}
			m.db.DeleteProcessRecord(r.RepoID, r.BeHash)
		}
	}
	m.removeLabeled("", "")
}

// PurgeRepoContainers removes every container and network labeled for the
// repo — repo deletion's container-side cleanup, catching leftovers no
// longer in the in-memory table.
func (m *Manager) PurgeRepoContainers(repoName string) {
	m.removeLabeled("local-preview.repo", repoName)
}

// removeLabeled force-removes managed containers (then networks) matching
// the label filter; an empty label sweeps everything managed.
func (m *Manager) removeLabeled(label, value string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cli, err := m.docker(ctx)
	if err != nil {
		return
	}
	if label == "" {
		label = dockerapi.ManagedLabel
		value = ""
	}
	if cs, err := cli.ListContainersByLabel(ctx, label, value); err == nil {
		for _, c := range cs {
			cli.RemoveContainer(ctx, c.ID, true) //nolint:errcheck
		}
	}
	if ns, err := cli.ListNetworksByLabel(ctx, label, value); err == nil {
		for _, n := range ns {
			cli.RemoveNetwork(ctx, n.ID) //nolint:errcheck
		}
	}
}

// ForkOrInitStateDir provisions the state directory for a newly built
// backend artifact and records the backend_artifacts row. It walks
// first-parent ancestry for the nearest ready deploy whose backend has a
// live state dir, briefly stops that backend (a full stop is the only
// quiesce that needs no assumptions about the app's persistence engine),
// copies, and renames into place. With no usable ancestor the state starts
// fresh. Idempotent: an existing backend_artifacts row short-circuits.
func (m *Manager) ForkOrInitStateDir(ctx context.Context, git gitrepo.Repo, repoID int64, repoName, newBeHash, sha, runConfigJSON string) error {
	if _, err := m.db.GetBackendArtifact(repoID, newBeHash); err == nil {
		// Already provisioned. Refresh the run contract: `networks` isn't
		// part of the artifact hash, so its changes land here rather than
		// in a new row.
		return m.db.UpdateBackendArtifactRunConfig(repoID, newBeHash, runConfigJSON)
	} else if !errors.Is(err, db.ErrNotFound) {
		return err
	}

	forkedFrom := ""
	if src := m.findForkSource(ctx, git, repoID, repoName, newBeHash, sha); src != "" {
		srcKey := BackendKey(repoID, src)
		lock := m.keyLock(srcKey)
		lock.Lock()
		m.stopLocked(srcKey, "quiesce for state fork")
		err := m.files.ForkStateDir(repoName, src, newBeHash)
		lock.Unlock()
		if err != nil {
			return err
		}
		forkedFrom = src
	} else if err := m.files.InitFreshStateDir(repoName, newBeHash); err != nil {
		return err
	}

	return m.db.CreateBackendArtifact(db.BackendArtifact{
		RepoID:     repoID,
		BeHash:     newBeHash,
		ForkedFrom: forkedFrom,
		StateDir:   m.files.StateDirPath(repoName, newBeHash),
		RunConfig:  runConfigJSON,
	})
}

// findForkSource returns the be_hash of the nearest deployed ancestor with
// a live state dir, or "".
func (m *Manager) findForkSource(ctx context.Context, git gitrepo.Repo, repoID int64, repoName, newBeHash, sha string) string {
	ancestors, err := git.FirstParentAncestry(ctx, sha, AncestryLimit)
	if err != nil {
		return ""
	}
	for _, anc := range ancestors {
		d, err := m.db.GetDeployBySHA(repoID, anc)
		if err != nil || d.Status != db.DeployReady || d.BeHash == "" || d.BeHash == newBeHash {
			continue
		}
		if _, err := m.db.GetBackendArtifact(repoID, d.BeHash); err != nil {
			continue
		}
		if !m.files.HasStateDir(repoName, d.BeHash) {
			continue
		}
		return d.BeHash
	}
	return ""
}

func (m *Manager) openRunLog(repoName, beHash string) (*os.File, error) {
	dir := filepath.Join(m.logsDir, repoName, "run", beHash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	entries, _ := os.ReadDir(dir)
	name := strconv.Itoa(len(entries)+1) + ".log"
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open run log: %w", err)
	}
	return f, nil
}

// probeFreePort asks the OS for a free loopback port. The tiny window
// between Close and the child's bind is accepted: a bind failure surfaces
// as a fast exit and the retry probes a fresh port.
func probeFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("probe free port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// templateArgv substitutes {port} and {state_dir} in each argv element.
func templateArgv(argv []string, port int, stateDir string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		a = strings.ReplaceAll(a, "{port}", strconv.Itoa(port))
		a = strings.ReplaceAll(a, "{state_dir}", stateDir)
		out[i] = a
	}
	return out
}
