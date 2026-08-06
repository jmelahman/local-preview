// Package client is a small HTTP client for the app's REST API, used by the
// CLI subcommands in cmd/server.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Repo mirrors the API's repo shape.
type Repo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Source        string `json:"source"`
	Watch         bool   `json:"watch"`
	WatchBranches string `json:"watch_branches"`
	CreatedAt     string `json:"created_at"`
}

// Deploy mirrors the API's deploy shape.
type Deploy struct {
	ID           int64  `json:"id"`
	Repo         string `json:"repo"`
	SHA          string `json:"sha"`
	ShortSHA     string `json:"short_sha"`
	Ref          string `json:"ref"`
	Branch       string `json:"branch"`
	AuthorName   string `json:"author_name"`
	AuthorEmail  string `json:"author_email"`
	FeHash       string `json:"fe_hash"`
	BeHash       string `json:"be_hash"`
	Status       string `json:"status"`
	Error        string `json:"error"`
	AttemptCount int64  `json:"attempt_count"`
	PreviewURL   string `json:"preview_url"`
	Process      string `json:"process"`
	FeProcess    string `json:"fe_process"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// Client talks to a running `preview serve` over HTTP.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for the server at base. Pass nil to use
// http.DefaultClient.
func New(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: strings.TrimRight(base, "/"), http: hc}
}

// CreateRepo registers a repo, optionally watched from the start.
func (c *Client) CreateRepo(ctx context.Context, name, source string, watch bool, watchBranches string) (Repo, error) {
	body, err := json.Marshal(map[string]any{
		"name": name, "source": source,
		"watch": watch, "watch_branches": watchBranches,
	})
	if err != nil {
		return Repo{}, err
	}
	raw, err := c.do(ctx, http.MethodPost, "/api/repos", body)
	if err != nil {
		return Repo{}, err
	}
	var r Repo
	if err := json.Unmarshal(raw, &r); err != nil {
		return Repo{}, fmt.Errorf("decode repo: %w", err)
	}
	return r, nil
}

// SetRepoWatch enables or disables watching for a repo. branches == nil
// leaves the stored branch filter unchanged.
func (c *Client) SetRepoWatch(ctx context.Context, name string, watch bool, branches *string) (Repo, error) {
	payload := map[string]any{"watch": watch}
	if branches != nil {
		payload["watch_branches"] = *branches
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Repo{}, err
	}
	raw, err := c.do(ctx, http.MethodPatch, "/api/repos/"+url.PathEscape(name), body)
	if err != nil {
		return Repo{}, err
	}
	var r Repo
	if err := json.Unmarshal(raw, &r); err != nil {
		return Repo{}, fmt.Errorf("decode repo: %w", err)
	}
	return r, nil
}

// ListRepos fetches all registered repos.
func (c *Client) ListRepos(ctx context.Context) ([]Repo, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/repos", nil)
	if err != nil {
		return nil, err
	}
	var repos []Repo
	if err := json.Unmarshal(raw, &repos); err != nil {
		return nil, fmt.Errorf("decode repos: %w", err)
	}
	return repos, nil
}

// DeleteRepo unregisters a repo and deletes its deploys, artifacts, state,
// and mirror clone.
func (c *Client) DeleteRepo(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/repos/"+url.PathEscape(name), nil)
	return err
}

// CreateDeploy requests a deploy of ref in repo.
func (c *Client) CreateDeploy(ctx context.Context, repo, ref string, rebuild bool) (Deploy, error) {
	body, err := json.Marshal(map[string]any{"repo": repo, "ref": ref, "rebuild": rebuild})
	if err != nil {
		return Deploy{}, err
	}
	raw, err := c.do(ctx, http.MethodPost, "/api/deploys", body)
	if err != nil {
		return Deploy{}, err
	}
	var d Deploy
	if err := json.Unmarshal(raw, &d); err != nil {
		return Deploy{}, fmt.Errorf("decode deploy: %w", err)
	}
	return d, nil
}

// DeployFilter narrows ListDeploys; zero-value fields don't filter. It
// mirrors the API's query params: repo and branch match exactly, author is a
// case-insensitive substring of the author name or email.
type DeployFilter struct {
	Repo   string
	Branch string
	Author string
}

// ListDeploys fetches deploys narrowed by the filter.
func (c *Client) ListDeploys(ctx context.Context, f DeployFilter) ([]Deploy, error) {
	q := url.Values{}
	for key, val := range map[string]string{"repo": f.Repo, "branch": f.Branch, "author": f.Author} {
		if val != "" {
			q.Set(key, val)
		}
	}
	path := "/api/deploys"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var deploys []Deploy
	if err := json.Unmarshal(raw, &deploys); err != nil {
		return nil, fmt.Errorf("decode deploys: %w", err)
	}
	return deploys, nil
}

// GetDeploy fetches one deploy by id.
func (c *Client) GetDeploy(ctx context.Context, id int64) (Deploy, error) {
	raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/deploys/%d", id), nil)
	if err != nil {
		return Deploy{}, err
	}
	var d Deploy
	if err := json.Unmarshal(raw, &d); err != nil {
		return Deploy{}, fmt.Errorf("decode deploy: %w", err)
	}
	return d, nil
}

// GetDeployLogs fetches the plain-text build log snapshot.
func (c *Client) GetDeployLogs(ctx context.Context, id int64) (string, error) {
	raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/deploys/%d/logs", id), nil)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// do performs a request and returns the response body, converting non-2xx
// responses into errors using the API's {"error": "..."} body when present.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s %s: %s", method, path, e.Error)
		}
		return nil, fmt.Errorf("%s %s: unexpected status %d", method, path, resp.StatusCode)
	}
	return raw, nil
}
