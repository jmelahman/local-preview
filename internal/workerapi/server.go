package workerapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/jmelahman/local-preview/internal/supervise"
)

// Supervisor is the worker-local orchestration surface the API exposes. It is
// exactly the subset of supervise.Manager the control node drives remotely, so
// *supervise.Manager satisfies it and tests can substitute a fake.
type Supervisor interface {
	EnsureRunning(ctx context.Context, k supervise.Key, repoName string) (int, error)
	Stop(k supervise.Key, reason string)
	Status(k supervise.Key) string
	Running() int
	MaxWarm() int
}

// Server exposes a Supervisor over HTTP behind a shared-secret check. Mount
// Handler() on a private listener only.
type Server struct {
	sup      Supervisor
	secret   string
	draining atomic.Bool
}

// NewServer wraps sup.
func NewServer(sup Supervisor, secret string) *Server {
	return &Server{sup: sup, secret: secret}
}

// SetDraining marks the worker draining (or not). A draining worker keeps
// serving what is already warm but reports Draining in its heartbeat so the
// control node's placement stops sending it new work — the pre-terminate half
// of an ASG scale-in lifecycle hook.
func (s *Server) SetDraining(v bool) { s.draining.Store(v) }

// Handler returns the authenticated worker-API mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathEnsure, s.handleEnsure)
	mux.HandleFunc(pathStop, s.handleStop)
	mux.HandleFunc(pathStatus, s.handleStatus)
	mux.HandleFunc(pathHeartbeat, s.handleHeartbeat)
	mux.HandleFunc(pathDrain, s.handleDrain)
	return s.authed(mux)
}

// authed rejects any request without the shared secret, in constant time.
func (s *Server) authed(next http.Handler) http.Handler {
	want := "Bearer " + s.secret
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(AuthHeader)
		if s.secret == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleEnsure(w http.ResponseWriter, r *http.Request) {
	var req ensureReq
	if !decode(w, r, &req) {
		return
	}
	port, err := s.sup.EnsureRunning(r.Context(), req.Key.toKey(), req.Repo)
	if err != nil {
		// A failed start is a normal outcome (crash, missing artifact), reported
		// to the control node as a 502 with the detail so its proxy can render
		// the same "failed to start" page a local start would.
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, ensureResp{Port: port})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req stopReq
	if !decode(w, r, &req) {
		return
	}
	s.sup.Stop(req.Key.toKey(), req.Reason)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var req statusReq
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, statusResp{Status: s.sup.Status(req.Key.toKey())})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, Heartbeat{
		Running:  s.sup.Running(),
		MaxWarm:  s.sup.MaxWarm(),
		Draining: s.draining.Load(),
	})
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	var req drainReq
	if !decode(w, r, &req) {
		return
	}
	s.draining.Store(req.Draining)
	w.WriteHeader(http.StatusNoContent)
}

// maxWorkerBody caps request bodies before decoding. Every request shape here
// is tiny (a key plus a flag or two), so a low cap is ample and keeps a
// compromised or misconfigured-network peer from forcing unbounded allocation
// against this RCE-adjacent surface.
const maxWorkerBody = 64 << 10 // 64 KiB

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkerBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
