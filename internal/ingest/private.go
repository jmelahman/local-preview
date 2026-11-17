package ingest

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/db"
)

type Lease struct {
	ID            string              `json:"id"`
	Spec          build.ExecutionSpec `json:"spec"`
	ArchiveBytes  int64               `json:"archive_bytes"`
	ArchiveSHA256 string              `json:"archive_sha256"`
	Token         string              `json:"lease_token"`
	Until         int64               `json:"lease_until"`
	ExpiresAt     int64               `json:"expires_at"`
}

func (c *Coordinator) Handler(secret string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ingestions/claim", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WorkerID string `json:"worker_id"`
		}
		if !privateDecode(w, r, &req) {
			return
		}
		if req.WorkerID == "" || len(req.WorkerID) > 128 {
			http.Error(w, "invalid worker identity", http.StatusBadRequest)
			return
		}
		token, err := randomID()
		if err != nil {
			privateError(w, err)
			return
		}
		job, err := c.Store.ClaimJob(req.WorkerID, token, time.Now(), LeaseDuration, MaxAttempts)
		if errors.Is(err, db.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			privateError(w, err)
			return
		}
		var snapshot Snapshot
		if err := json.Unmarshal([]byte(job.SpecJSON), &snapshot); err != nil {
			privateError(w, err)
			return
		}
		privateJSON(w, Lease{ID: job.ID, Spec: snapshot.Execution, ArchiveBytes: job.ArchiveBytes, ArchiveSHA256: job.ArchiveSHA256, Token: job.LeaseToken, Until: job.LeaseUntil, ExpiresAt: job.ExpiresAt})
	})
	mux.HandleFunc("POST /v1/ingestions/{id}/renew", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"lease_token"`
		}
		if !privateDecode(w, r, &req) {
			return
		}
		if err := c.Store.RenewJob(r.PathValue("id"), req.Token, time.Now(), LeaseDuration); err != nil {
			privateError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/ingestions/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token      string     `json:"lease_token"`
			Completion Completion `json:"completion"`
		}
		if !privateDecode(w, r, &req) {
			return
		}
		if err := c.Complete(r.PathValue("id"), req.Token, req.Completion); err != nil {
			privateError(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /v1/ingestions/{id}/fail", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"lease_token"`
			Error string `json:"error"`
			Retry bool   `json:"retry"`
		}
		if !privateDecode(w, r, &req) {
			return
		}
		if len(req.Error) > 8192 {
			req.Error = req.Error[:8192]
		}
		if err := c.Store.FailJobAttempt(r.PathValue("id"), req.Token, req.Error, req.Retry, time.Now(), MaxAttempts); err != nil {
			privateError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if secret == "" || !ok || subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func privateDecode(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxMessageBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, "invalid job request", http.StatusBadRequest)
		return false
	}
	return true
}

func privateJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func privateError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, db.ErrJobLeaseLost) || errors.Is(err, db.ErrJobNotReady) {
		status = http.StatusConflict
	}
	if errors.Is(err, db.ErrNotFound) {
		status = http.StatusNotFound
	}
	if errors.Is(err, ErrValidation) {
		status = http.StatusBadRequest
	}
	http.Error(w, http.StatusText(status), status)
}
