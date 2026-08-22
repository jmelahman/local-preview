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

	// SpecResolver resolves the control-side run spec shipped with each
	// ensure request (plus the peer backend's, for a process-mode frontend) —
	// supervise.(*Manager).ResolveWireSpec in production. Nil sends none,
	// which only serves against a worker whose own DB knows the artifact;
	// outside tests that means every ensure fails "not provisioned".
	SpecResolver func(k supervise.Key) (supervise.WireSpec, error)
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
	req := ensureReq{Key: fromKey(k), Repo: repoName}
	if c.SpecResolver != nil {
		spec, err := c.SpecResolver(k)
		if err != nil {
			// Same failure a local start would report — don't bother the worker.
			return "", err
		}
		req.Spec = &spec
		if k.Side == supervise.SideFrontend && k.Peer != "" {
			peer, err := c.SpecResolver(supervise.BackendKey(k.RepoID, k.Peer))
			if err != nil {
				return "", err
			}
			req.PeerSpec = &peer
		}
	}
	var resp ensureResp
	if err := c.post(ctx, pathEnsure, req, &resp); err != nil {
		return "", err
	}
	return c.host + ":" + strconv.Itoa(resp.Port), nil
}

// Stop asks the worker to stop the keyed process.
func (c *Client) Stop(ctx context.Context, k supervise.Key, reason string) error {
	return c.post(ctx, pathStop, stopReq{Key: fromKey(k), Reason: reason}, nil)
}

// Status reads the keyed process's status from the worker, with the failure
// detail when it is "crashed".
func (c *Client) Status(ctx context.Context, k supervise.Key) (status, detail string, err error) {
	var resp statusResp
	if err := c.post(ctx, pathStatus, statusReq{Key: fromKey(k)}, &resp); err != nil {
		return "", "", err
	}
	return resp.Status, resp.Error, nil
}

// Report fetches the worker's full live-state dump: every non-idle key's
// status, failure detail, and resource sample.
func (c *Client) Report(ctx context.Context) ([]supervise.ProcReport, error) {
	var resp reportResp
	if err := c.post(ctx, pathReport, struct{}{}, &resp); err != nil {
		return nil, err
	}
	out := make([]supervise.ProcReport, 0, len(resp.Procs))
	for _, p := range resp.Procs {
		out = append(out, supervise.ProcReport{
			Key: p.Key.toKey(), Repo: p.Repo, Status: p.Status, Error: p.Error, Stats: p.Stats,
		})
	}
	return out, nil
}

// RunLog fetches an incremental run-log slice from the worker's disk.
func (c *Client) RunLog(ctx context.Context, repo, side, hash string, attempt int, offset int64) (supervise.RunLog, error) {
	var chunk supervise.RunLog
	err := c.post(ctx, pathRunLog, runLogReq{Repo: repo, Side: side, Hash: hash, Attempt: attempt, Offset: offset}, &chunk)
	return chunk, err
}

// Drain marks the worker draining (or clears it) — used by a control-node
// lifecycle handler winding a worker down before its instance terminates.
func (c *Client) Drain(ctx context.Context, draining bool) error {
	return c.post(ctx, pathDrain, drainReq{Draining: draining}, nil)
}

// Configure pushes runtime settings to the worker — the dashboard-edited
// warm cap and idle-timeout override. The control node's heartbeat loop
// re-pushes after a worker reboot, so the settings survive the fleet.
func (c *Client) Configure(ctx context.Context, cfg WorkerConfig) error {
	return c.post(ctx, pathConfigure, cfg, nil)
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
	return postJSON(ctx, c.hc, c.base+path, c.secret, body, out)
}

// postJSON POSTs body as JSON to url with the shared-secret bearer header and
// decodes an optional JSON response. A non-2xx carries the peer's error text.
// Shared by both directions of the protocol (control→worker and worker→control).
func postJSON(ctx context.Context, hc *http.Client, url, secret string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set(AuthHeader, "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	res, err := hc.Do(req)
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

// ControlClient is the worker-side transport to the control node's registration
// endpoint — the reverse direction of Client. A self-registering worker calls
// Register on boot and periodically (so a restarted control node re-learns it),
// and Deregister on graceful shutdown.
type ControlClient struct {
	base   string // control node's control-API base URL, e.g. http://10.0.1.1:9101
	secret string
	hc     *http.Client
}

// NewControlClient dials the control node's registration API. A nil httpClient
// gets a default with a sane timeout.
func NewControlClient(baseURL, secret string, hc *http.Client) *ControlClient {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &ControlClient{base: baseURL, secret: secret, hc: hc}
}

// Register announces this worker to the control node: endpoint is this worker's
// own worker-API base URL, host the routable host its processes serve on, and
// instanceID its cloud instance-id (empty if it has none) so the control node
// can scale-in-protect it while busy.
func (c *ControlClient) Register(ctx context.Context, endpoint, host, instanceID string) error {
	return postJSON(ctx, c.hc, c.base+pathRegister, c.secret, registerReq{Endpoint: endpoint, Host: host, InstanceID: instanceID}, nil)
}

// Deregister removes this worker from the control node's fleet.
func (c *ControlClient) Deregister(ctx context.Context, endpoint string) error {
	return postJSON(ctx, c.hc, c.base+pathDeregister, c.secret, deregisterReq{Endpoint: endpoint}, nil)
}
