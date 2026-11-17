package ingest

import (
	"context"
	"net/http"
	"time"
)

func (w *Worker) maintainLease(ctx context.Context, lease Lease, cancel context.CancelFunc, stop <-chan struct{}, interval time.Duration) {
	until := time.Unix(lease.Until, 0)
	expired := time.NewTimer(time.Until(until))
	defer expired.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-expired.C:
			cancel()
			return
		case <-ticker.C:
			started := time.Now()
			renewal, stopRequest := context.WithDeadline(ctx, until)
			status, err := w.request(renewal, "/v1/ingestions/"+lease.ID+"/renew", map[string]string{"lease_token": lease.Token}, nil)
			stopRequest()
			if status == http.StatusNotFound || status == http.StatusConflict || !time.Now().Before(until) {
				cancel()
				return
			}
			if err != nil {
				continue
			}
			until = started.Truncate(time.Second).Add(LeaseDuration)
			if expires := time.Unix(lease.ExpiresAt, 0); expires.Before(until) {
				until = expires
			}
			expired.Reset(time.Until(until))
		}
	}
}
