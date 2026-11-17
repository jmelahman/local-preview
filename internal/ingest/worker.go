package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/s3store"
)

type Worker struct {
	ServerURL     string
	Secret        string
	ID            string
	DataDir       string
	Tier          *s3store.Tier
	Executor      build.Executor
	HTTP          *http.Client
	IdleTimeout   time.Duration
	BeforeJob     func(context.Context) error
	AfterJob      func(context.Context) error
	OnIdle        func(context.Context) error
	CheckCapacity func(context.Context) error
}

func (w *Worker) Run(ctx context.Context) error {
	if w.HTTP == nil {
		w.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if w.ServerURL == "" || w.Secret == "" || w.ID == "" {
		return errors.New("ingestion worker server, secret, and identity are required")
	}
	if w.DataDir == "" || w.Executor.DataDir == "" || filepath.Clean(w.DataDir) != filepath.Clean(w.Executor.DataDir) {
		return errors.New("ingestion worker and executor must use the same nonempty data directory")
	}
	if err := os.MkdirAll(w.DataDir, 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(w.Executor.DataDir, "jobs")); err != nil {
		return err
	}
	idleSince := time.Now()
	for {
		if w.CheckCapacity != nil {
			if err := w.CheckCapacity(ctx); err != nil {
				if w.OnIdle != nil {
					return w.OnIdle(ctx)
				}
				return err
			}
		}
		var lease Lease
		status, err := w.request(ctx, "/v1/ingestions/claim", map[string]string{"worker_id": w.ID}, &lease)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.Printf("ingestion: claim: %v", err)
		}
		if err == nil && status == http.StatusOK {
			if err := w.execute(ctx, lease); err != nil {
				log.Printf("ingestion: job %s: %v", lease.ID, err)
			}
			idleSince = time.Now()
			continue
		}
		if err == nil && w.OnIdle != nil && w.IdleTimeout > 0 && time.Since(idleSince) >= w.IdleTimeout {
			if err := w.OnIdle(ctx); err == nil {
				return nil
			} else {
				log.Printf("ingestion: idle termination: %v", err)
				idleSince = time.Now()
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (w *Worker) execute(parent context.Context, lease Lease) (retErr error) {
	ctx, cancel := context.WithDeadline(parent, time.Unix(lease.ExpiresAt, 0))
	defer cancel()
	if w.BeforeJob != nil {
		if err := w.BeforeJob(ctx); err != nil {
			w.fail(lease, err, true)
			return err
		}
	}
	if w.AfterJob != nil {
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if err := w.AfterJob(cleanup); err != nil {
				log.Printf("ingestion: release worker protection: %v", err)
			}
		}()
	}
	stopRenew := make(chan struct{})
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		w.maintainLease(ctx, lease, cancel, stopRenew, LeaseDuration/4)
	}()
	defer func() { close(stopRenew); <-renewed }()
	var archive *os.File
	if lease.Spec.Side != "" {
		reader, err := w.Tier.OpenUpload(ctx, lease.ID)
		if err != nil {
			w.fail(lease, err, true)
			return err
		}
		defer reader.Close()
		archive, err = os.CreateTemp(w.DataDir, "input-*")
		if err != nil {
			w.fail(lease, err, true)
			return err
		}
		defer func() { archive.Close(); os.Remove(archive.Name()) }()
		hash := sha256.New()
		size, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(reader, lease.ArchiveBytes+1))
		if err != nil {
			w.fail(lease, err, true)
			return err
		}
		if size != lease.ArchiveBytes || hex.EncodeToString(hash.Sum(nil)) != lease.ArchiveSHA256 {
			err := errors.New("staged archive size or SHA-256 does not match the accepted job")
			w.fail(lease, err, false)
			return err
		}
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			w.fail(lease, err, true)
			return err
		}
	}
	tier, err := w.Tier.Attempt(lease.ID, lease.Token)
	if err != nil {
		w.fail(lease, err, false)
		return err
	}
	var input io.Reader
	if archive != nil {
		input = archive
	}
	result, err := w.Executor.Execute(ctx, lease.Spec, input, tier)
	if err != nil {
		w.fail(lease, err, ctx.Err() != nil)
		return err
	}
	completion := Completion{Execution: result, Outputs: tier.ListOutputs()}
	status, err := w.request(ctx, "/v1/ingestions/"+lease.ID+"/complete", struct {
		Token      string     `json:"lease_token"`
		Completion Completion `json:"completion"`
	}{lease.Token, completion}, nil)
	if err != nil {
		w.fail(lease, err, status == 0 || status >= 500)
	}
	return err
}

func (w *Worker) fail(lease Lease, err error, retry bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, reportErr := w.request(ctx, "/v1/ingestions/"+lease.ID+"/fail", struct {
		Token string `json:"lease_token"`
		Error string `json:"error"`
		Retry bool   `json:"retry"`
	}{lease.Token, err.Error(), retry}, nil)
	if reportErr != nil {
		log.Printf("ingestion: record job %s failure: %v", lease.ID, reportErr)
	}
}

func (w *Worker) request(ctx context.Context, path string, value, result any) (int, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	if len(data) > MaxMessageBytes {
		return 0, errors.New("ingestion metadata exceeds the 4 MiB limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.ServerURL+path, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+w.Secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("ingestion coordinator returned HTTP %d", resp.StatusCode)
	}
	if result != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(resp.Body, MaxMessageBytes)).Decode(result); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}
