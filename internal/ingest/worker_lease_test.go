package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseRenewalSurvivesTransientCoordinatorFailure(t *testing.T) {
	var requests atomic.Int32
	recovered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusNoContent)
			close(recovered)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	worker := Worker{ServerURL: server.URL, Secret: "test", HTTP: server.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.maintainLease(ctx, Lease{ID: "job", Token: "token", Until: time.Now().Add(5 * time.Second).Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix()}, cancel, stop, 10*time.Millisecond)
	}()
	select {
	case <-recovered:
	case <-ctx.Done():
		t.Fatal("a transient coordinator failure cancelled the build")
	case <-time.After(time.Second):
		t.Fatal("worker did not retry lease renewal")
	}
	if ctx.Err() != nil {
		t.Fatal("build was cancelled after recovery")
	}
	close(stop)
	<-done
}

func TestLeaseRenewalStopsOnLostOrExpiredLease(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			worker := Worker{ServerURL: server.URL, Secret: "test", HTTP: server.Client()}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				worker.maintainLease(ctx, Lease{ID: "job", Token: "token", Until: time.Now().Add(time.Second).Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix()}, cancel, make(chan struct{}), 10*time.Millisecond)
			}()
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("worker kept running without a valid lease")
			}
			<-done
		})
	}
}
