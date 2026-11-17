package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"
)

type HTTPError struct {
	StatusCode int
	RetryAfter string
	Message    string
}

func (e *HTTPError) Error() string { return e.Message }

type StagedUploadResult struct {
	Upload UploadResult `json:"upload"`
	Deploy *Deploy      `json:"deploy,omitempty"`
}

type ingestionResponse struct {
	ID           string              `json:"id"`
	Status       string              `json:"status"`
	Error        string              `json:"error"`
	UploadURL    string              `json:"upload_url"`
	UploadFields map[string]string   `json:"upload_fields"`
	Result       *StagedUploadResult `json:"result,omitempty"`
}

func (c *Client) StagedUpload(ctx context.Context, repo, side, name, ref string, overwrite, deploy bool, archivePath string) (result StagedUploadResult, retErr error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	archive, err := os.Open(archivePath)
	if err != nil {
		return result, err
	}
	defer archive.Close()
	stat, err := archive.Stat()
	if err != nil {
		return result, err
	}
	if !stat.Mode().IsRegular() {
		return result, errors.New("staged upload requires a regular archive file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, &contextReader{ctx: ctx, reader: archive}); err != nil {
		return result, err
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return result, err
	}
	key[6] = key[6]&0x0f | 0x40
	key[8] = key[8]&0x3f | 0x80
	requestKey := fmt.Sprintf("%x-%x-%x-%x-%x", key[:4], key[4:6], key[6:8], key[8:10], key[10:])
	payload, err := json.Marshal(struct {
		RequestKey    string `json:"request_key"`
		Ref           string `json:"ref"`
		Side          string `json:"side"`
		Name          string `json:"name,omitempty"`
		Overwrite     bool   `json:"overwrite"`
		Deploy        bool   `json:"deploy"`
		ArchiveSize   int64  `json:"archive_size"`
		ArchiveSHA256 string `json:"archive_sha256"`
	}{requestKey, ref, side, name, overwrite, deploy, stat.Size(), hex.EncodeToString(h.Sum(nil))})
	if err != nil {
		return result, err
	}
	base := "/api/repos/" + url.PathEscape(repo) + "/ingestions"
	var job ingestionResponse
	raw, err := c.ingestionRequest(ctx, http.MethodPost, base, payload)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return result, fmt.Errorf("decode ingestion: %w", err)
	}
	if job.ID == "" {
		return result, errors.New("ingestion response has no job ID")
	}
	path := base + "/" + url.PathEscape(job.ID)
	defer func() {
		if retErr != nil && ctx.Err() != nil {
			c.cancelIngestion(ctx, path)
		}
	}()
	if job.Status == "uploading" {
		if job.UploadURL == "" {
			return result, errors.New("ingestion response has no upload URL")
		}
		if err := c.uploadArchive(ctx, job.UploadURL, job.UploadFields, archive, stat.Size()); err != nil {
			return result, err
		}
		raw, err = c.ingestionRequest(ctx, http.MethodPost, path+"/submit", []byte("{}"))
		if err != nil {
			return result, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &job); err != nil {
				return result, fmt.Errorf("decode submitted ingestion: %w", err)
			}
		} else {
			job.Status = "queued"
		}
	}
	return c.waitForIngestion(ctx, path, job, deploy)
}

func (c *Client) cancelIngestion(ctx context.Context, path string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, _ = c.do(cleanupCtx, http.MethodDelete, path, nil)
}

func (c *Client) waitForIngestion(ctx context.Context, path string, job ingestionResponse, requireDeploy bool) (StagedUploadResult, error) {
	for {
		switch job.Status {
		case "done":
			if job.Result == nil {
				return StagedUploadResult{}, errors.New("completed ingestion has no result")
			}
			if requireDeploy && (job.Result.Deploy == nil || job.Result.Deploy.Status != "ready" || job.Result.Deploy.PreviewURL == "") {
				return StagedUploadResult{}, errors.New("completed ingestion has no ready deploy")
			}
			return *job.Result, nil
		case "failed":
			return StagedUploadResult{}, fmt.Errorf("ingestion %s failed: %s", job.ID, job.Error)
		case "cancelled":
			return StagedUploadResult{}, fmt.Errorf("ingestion %s cancelled", job.ID)
		case "queued", "leased", "publishing":
		default:
			return StagedUploadResult{}, fmt.Errorf("ingestion %s has unexpected status %q", job.ID, job.Status)
		}
		if err := waitIngestion(ctx, time.Second); err != nil {
			return StagedUploadResult{}, err
		}
		raw, err := c.ingestionRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			return StagedUploadResult{}, err
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			return StagedUploadResult{}, fmt.Errorf("decode ingestion status: %w", err)
		}
	}
}

func (c *Client) ingestionRequest(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		raw, err := c.do(ctx, method, path, body)
		var httpErr *HTTPError
		if err == nil || !errors.As(err, &httpErr) || (httpErr.StatusCode != http.StatusTooManyRequests && httpErr.StatusCode != http.StatusServiceUnavailable) {
			return raw, err
		}
		delay := time.Second << min(attempt, 5)
		if seconds, e := strconv.Atoi(httpErr.RetryAfter); e == nil && seconds >= 0 {
			delay = time.Duration(min(seconds, 30*60)) * time.Second
		} else if at, e := http.ParseTime(httpErr.RetryAfter); e == nil {
			delay = max(time.Until(at), time.Duration(0))
		}
		if err := waitIngestion(ctx, max(delay, 100*time.Millisecond)); err != nil {
			return nil, err
		}
	}
}

func waitIngestion(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) uploadArchive(ctx context.Context, target string, fields map[string]string, archive *os.File, size int64) error {
	var header bytes.Buffer
	form := multipart.NewWriter(&header)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := form.WriteField(key, fields[key]); err != nil {
			return err
		}
	}
	if _, err := form.CreateFormFile("file", "upload.tar.gz"); err != nil {
		return err
	}
	prefix := append([]byte(nil), header.Bytes()...)
	header.Reset()
	if err := form.Close(); err != nil {
		return err
	}
	suffix := append([]byte(nil), header.Bytes()...)
	for attempt := range 3 {
		body := io.MultiReader(bytes.NewReader(prefix), io.NewSectionReader(archive, 0, size), bytes.NewReader(suffix))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
		if err != nil {
			return errors.New("invalid staged upload URL")
		}
		req.Header.Set("Content-Type", form.FormDataContentType())
		req.ContentLength = int64(len(prefix)) + size + int64(len(suffix))
		resp, err := c.http.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return fmt.Errorf("staged archive upload returned HTTP %d", resp.StatusCode)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt == 2 {
			return errors.New("staged archive upload failed after three attempts")
		}
		if err := waitIngestion(ctx, time.Duration(attempt+1)*time.Second); err != nil {
			return err
		}
	}
	return errors.New("staged archive upload failed")
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
