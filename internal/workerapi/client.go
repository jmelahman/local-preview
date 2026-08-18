package workerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jmelahman/local-preview/internal/supervise"
)

// Client is the control-node transport to one worker. Its EnsureRunning
// satisfies proxy.Backends, so the proxy routes to a remote worker exactly as
// it routes to a local Manager — the "one orchestrator, two transports"
// property. host is the worker's routable address (how the control node reaches
// the worker's supervised processes); the worker returns only the port.
type Client struct {
	base   string // worker-API base URL, e.g. http://10.0.1.5:9100
	host   string // routable host for the worker's processes, e.g. 10.0.1.5
	secret string
	hc     *http.Client
}

// NewClient dials a worker. baseURL is its private worker-API URL; host is the
// address the control node reaches its processes on; secret is the shared
// secret. A nil httpClient gets a default with a sane timeout.
func NewClient(baseURL, host, secret string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: baseURL, host: host, secret: secret, hc: hc}
}

// EnsureRunning starts (or reuses) the keyed process on the worker and returns
// the "host:port" the control node's proxy dials.
func (c *Client) EnsureRunning(ctx context.Context, k supervise.Key, repoName string) (string, error) {
	var resp ensureResp
	if err := c.post(ctx, pathEnsure, ensureReq{Key: fromKey(k), Repo: repoName}, &resp); err != nil {
		return "", err
	}
	return c.host + ":" + strconv.Itoa(resp.Port), nil
}

// Stop asks the worker to stop the keyed process.
func (c *Client) Stop(ctx context.Context, k supervise.Key, reason string) error {
	return c.post(ctx, pathStop, stopReq{Key: fromKey(k), Reason: reason}, nil)
}

// Status reads the keyed process's status from the worker.
func (c *Client) Status(ctx context.Context, k supervise.Key) (string, error) {
	var resp statusResp
	if err := c.post(ctx, pathStatus, statusReq{Key: fromKey(k)}, &resp); err != nil {
		return "", err
	}
	return resp.Status, nil
}

// Drain marks the worker draining (or clears it) — used by a control-node
// lifecycle handler winding a worker down before its instance terminates.
func (c *Client) Drain(ctx context.Context, draining bool) error {
	return c.post(ctx, pathDrain, drainReq{Draining: draining}, nil)
}

// Heartbeat reads the worker's current capacity.
func (c *Client) Heartbeat(ctx context.Context) (Heartbeat, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+pathHeartbeat, nil)
	if err != nil {
		return Heartbeat{}, err
	}
	req.Header.Set(AuthHeader, "Bearer "+c.secret)
	res, err := c.hc.Do(req)
	if err != nil {
		return Heartbeat{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Heartbeat{}, fmt.Errorf("worker heartbeat: %s", res.Status)
	}
	var hb Heartbeat
	if err := json.NewDecoder(res.Body).Decode(&hb); err != nil {
		return Heartbeat{}, err
	}
	return hb, nil
}

// post sends a JSON body to path and decodes an optional JSON response. A
// non-2xx carries the worker's error text so the proxy renders the real reason.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set(AuthHeader, "Bearer "+c.secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("%s", bytes.TrimSpace(msg))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}
