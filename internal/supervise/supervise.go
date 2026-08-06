// Package supervise owns backend process lifecycles: one process per
// distinct backend artifact per repo, started on demand, health-checked,
// and killed as a process group. It also provisions backend state
// directories via lineage forking — the state for a new backend hash is
// copied from the nearest deployed ancestor's backend, quiesced first.
//
// The in-memory process table is authoritative while the orchestrator runs;
// process_records rows exist only so an unclean exit can be reclaimed at
// next startup.
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/manifest"
	"github.com/jmelahman/local-preview/internal/store"
)

// AncestryLimit caps how far the lineage walk searches for a fork source
// before falling back to a fresh state dir.
const AncestryLimit = 500

// stopGrace is how long a SIGTERM'd process gets before SIGKILL.
const stopGrace = 5 * time.Second

// Key identifies a supervised process: one per backend artifact per repo.
type Key struct {
	RepoID int64
	BeHash string
}

// Manager supervises backend processes.
type Manager struct {
	db      *db.Store
	files   *store.Store
	logsDir string

	healthInterval time.Duration

	mu    sync.Mutex
	procs map[Key]*process
	locks map[Key]*sync.Mutex
}

// New returns a Manager. logsDir is the root for per-process run logs.
func New(database *db.Store, files *store.Store, logsDir string) *Manager {
	return &Manager{
		db:             database,
		files:          files,
		logsDir:        logsDir,
		healthInterval: 200 * time.Millisecond,
		procs:          make(map[Key]*process),
		locks:          make(map[Key]*sync.Mutex),
	}
}

// process is one supervised child. ready is closed once healthy; failed is
// closed if the start attempt errors (err is set first). done is closed when
// the process exits, however that happens.
type process struct {
	repoName string
	port     int
	cmd      *exec.Cmd

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

// EnsureRunning returns the port of a healthy process for the backend
// artifact, starting one if needed. The start itself is never cancelled by
// ctx — a caller that gives up (e.g. a proxy request with a short deadline)
// leaves the start running for the next request to pick up.
func (m *Manager) EnsureRunning(ctx context.Context, repoID int64, repoName, beHash string) (int, error) {
	k := Key{RepoID: repoID, BeHash: beHash}
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
	return 0, fmt.Errorf("backend %s: process exited immediately after start", beHash[:12])
}

// forget removes p from the table if it's still the tracked entry.
func (m *Manager) forget(k Key, p *process) {
	m.mu.Lock()
	if m.procs[k] == p {
		delete(m.procs, k)
	}
	m.mu.Unlock()
}

// start runs the full start sequence: load run config, probe a port,
// exec, health-poll. It owns its own timeout and runs detached from any
// request context. Exactly one start goroutine exists per tracked process.
func (m *Manager) start(k Key, p *process) {
	lock := m.keyLock(k)
	lock.Lock()
	defer lock.Unlock()

	fail := func(event string, err error) {
		p.err = err
		m.db.AddProcessEvent(k.RepoID, k.BeHash, event, err.Error())
		close(p.failed)
		m.forget(k, p)
	}

	art, err := m.db.GetBackendArtifact(k.RepoID, k.BeHash)
	if err != nil {
		fail("start_attempt", fmt.Errorf("backend artifact %s not provisioned: %w", k.BeHash[:12], err))
		return
	}
	var cfg manifest.Backend
	if err := json.Unmarshal([]byte(art.RunConfig), &cfg); err != nil {
		fail("start_attempt", fmt.Errorf("parse run config: %w", err))
		return
	}
	backendDir := m.files.BackendDir(p.repoName, k.BeHash)
	if _, err := os.Stat(backendDir); err != nil {
		fail("start_attempt", fmt.Errorf("backend artifact files missing (evicted?): %w", err))
		return
	}

	port, err := probeFreePort()
	if err != nil {
		fail("start_attempt", err)
		return
	}
	p.port = port

	logFile, err := m.openRunLog(p.repoName, k.BeHash)
	if err != nil {
		fail("start_attempt", err)
		return
	}

	argv := templateArgv(cfg.Run, port, art.StateDir)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = backendDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		fail("start_attempt", fmt.Errorf("start backend: %w", err))
		return
	}
	p.cmd = cmd
	m.db.AddProcessEvent(k.RepoID, k.BeHash, "start_attempt",
		fmt.Sprintf("pid %d port %d", cmd.Process.Pid, port))
	m.db.UpsertProcessRecord(db.ProcessRecord{
		RepoID: k.RepoID, BeHash: k.BeHash,
		PID: cmd.Process.Pid, PGID: cmd.Process.Pid, Port: port,
	})

	// Reaper: closes done when the child exits and clears bookkeeping.
	go func() {
		waitErr := cmd.Wait()
		logFile.Close()
		close(p.done)
		m.forget(k, p)
		m.db.DeleteProcessRecord(k.RepoID, k.BeHash)
		if !p.intentional {
			detail := "exit ok"
			if waitErr != nil {
				detail = waitErr.Error()
			}
			m.db.AddProcessEvent(k.RepoID, k.BeHash, "exited", detail)
		}
	}()

	if err := m.awaitHealthy(p, cfg, port); err != nil {
		m.killGroup(cmd.Process.Pid)
		<-p.done
		event := "health_timeout"
		if isExited(p) {
			event = "exited"
		}
		fail(event, err)
		return
	}
	m.db.AddProcessEvent(k.RepoID, k.BeHash, "healthy", fmt.Sprintf("port %d", port))
	close(p.ready)
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
func (m *Manager) awaitHealthy(p *process, cfg manifest.Backend, port int) error {
	timeout := time.Duration(cfg.StartTimeout)
	if timeout <= 0 {
		timeout = manifest.DefaultStartTimeout
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(m.healthInterval)
	defer tick.Stop()
	client := &http.Client{Timeout: time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, cfg.HealthPath)
	for {
		select {
		case <-p.done:
			return fmt.Errorf("backend exited during startup (see run log)")
		case <-deadline:
			return fmt.Errorf("backend did not become healthy within %s", timeout)
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
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	p.intentional = true
	pid := p.cmd.Process.Pid
	syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(stopGrace):
		m.killGroup(pid)
		<-p.done
	}
	m.forget(k, p)
	m.db.AddProcessEvent(k.RepoID, k.BeHash, "idle_stop", reason)
}

func (m *Manager) killGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGKILL)
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

// Status reports the runtime state of a backend artifact's process for API
// views: "running", "starting", or "stopped".
func (m *Manager) Status(repoID int64, beHash string) string {
	m.mu.Lock()
	p := m.procs[Key{RepoID: repoID, BeHash: beHash}]
	m.mu.Unlock()
	if p == nil {
		return "stopped"
	}
	select {
	case <-p.ready:
		return "running"
	default:
		return "starting"
	}
}

// ReclaimOrphans handles process_records left by an unclean exit: if the
// recorded pid still leads a matching process group, the group is killed.
// PID reuse makes certain identification impossible; a health check always
// precedes routing, so a stale guess here can never misroute traffic.
func (m *Manager) ReclaimOrphans() {
	records, err := m.db.ListProcessRecords()
	if err != nil {
		return
	}
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

// ForkOrInitStateDir provisions the state directory for a newly built
// backend artifact and records the backend_artifacts row. It walks
// first-parent ancestry for the nearest ready deploy whose backend has a
// live state dir, briefly stops that backend (a full stop is the only
// quiesce that needs no assumptions about the app's persistence engine),
// copies, and renames into place. With no usable ancestor the state starts
// fresh. Idempotent: an existing backend_artifacts row short-circuits.
func (m *Manager) ForkOrInitStateDir(ctx context.Context, git gitrepo.Repo, repoID int64, repoName, newBeHash, sha, runConfigJSON string) error {
	if _, err := m.db.GetBackendArtifact(repoID, newBeHash); err == nil {
		return nil
	} else if !errors.Is(err, db.ErrNotFound) {
		return err
	}

	forkedFrom := ""
	if src := m.findForkSource(ctx, git, repoID, repoName, newBeHash, sha); src != "" {
		srcKey := Key{RepoID: repoID, BeHash: src}
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
