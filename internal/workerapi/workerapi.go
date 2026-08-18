// Package workerapi is the internal transport between a control node and an
// elastic worker. It is the *second transport* for the one orchestrator
// implementation: a worker runs the same supervise.Manager, and the control
// node reaches it through Client, which satisfies the same proxy.Backends
// interface the local loopback adapter does. Single-node (--role=all) is the
// degenerate case where no Client exists and serving stays loopback.
//
// The surface is deliberately tiny — exactly what the control node needs
// against a supervise.Key: start/reuse a process (Ensure), stop it, read its
// status, and a capacity heartbeat. It is a remote-code-execution surface by
// design (the worker starts arbitrary preview processes), so it is
// shared-secret authenticated and MUST live on a private listener that is never
// reachable from the ALB/internet.
package workerapi

import (
	"github.com/jmelahman/local-preview/internal/supervise"
)

// AuthHeader carries the shared secret ("Bearer <secret>"). The secret comes
// from SSM/config on both ends.
const AuthHeader = "Authorization"

// Route paths, versioned so the wire format can evolve.
const (
	pathEnsure    = "/internal/worker/v1/ensure"
	pathStop      = "/internal/worker/v1/stop"
	pathStatus    = "/internal/worker/v1/status"
	pathHeartbeat = "/internal/worker/v1/heartbeat"
	pathDrain     = "/internal/worker/v1/drain"
)

// WireKey is the JSON form of a supervise.Key crossing the boundary.
type WireKey struct {
	RepoID int64  `json:"repo_id"`
	Side   string `json:"side"`
	Hash   string `json:"hash"`
	Peer   string `json:"peer,omitempty"`
}

func fromKey(k supervise.Key) WireKey {
	return WireKey{RepoID: k.RepoID, Side: string(k.Side), Hash: k.Hash, Peer: k.Peer}
}

func (w WireKey) toKey() supervise.Key {
	return supervise.Key{RepoID: w.RepoID, Side: supervise.Side(w.Side), Hash: w.Hash, Peer: w.Peer}
}

// ensureReq/ensureResp: start or reuse a process, returning the port it serves
// on (loopback on the worker; the control-side Client pairs it with the
// worker's routable host).
type ensureReq struct {
	Key  WireKey `json:"key"`
	Repo string  `json:"repo"`
}
type ensureResp struct {
	Port int `json:"port"`
}

type stopReq struct {
	Key    WireKey `json:"key"`
	Reason string  `json:"reason"`
}

type statusReq struct {
	Key WireKey `json:"key"`
}
type statusResp struct {
	Status string `json:"status"`
}

type drainReq struct {
	Draining bool `json:"draining"`
}

// Heartbeat is the worker's capacity report, the input to fleet placement and
// scale-out. Draining marks a worker an ASG lifecycle hook is winding down: the
// control node routes new work elsewhere but keeps serving what is already warm.
type Heartbeat struct {
	Running  int  `json:"running"`
	MaxWarm  int  `json:"max_warm"`
	Draining bool `json:"draining"`
}
