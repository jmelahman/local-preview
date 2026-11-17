package s3store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"golang.org/x/sync/singleflight"
)

const StagingPrefix = "ingest-staging/"

var ingestionSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

type StagedUpload struct {
	URL    string            `json:"upload_url"`
	Fields map[string]string `json:"upload_fields"`
}

type StagedObject struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	ETag   string `json:"etag"`
}

type Output struct {
	Repo             string `json:"repo"`
	Side             string `json:"side"`
	Hash             string `json:"hash"`
	Key              string `json:"key"`
	ETag             string `json:"etag"`
	UncompressedSize int64  `json:"uncompressed_size"`
	FileCount        int64  `json:"file_count"`
	CompressedSize   int64  `json:"compressed_size"`
}

func uploadKey(jobID string) (string, error) {
	if !ingestionSegment.MatchString(jobID) {
		return "", errors.New("invalid ingestion job ID")
	}
	return StagingPrefix + jobID + "/input.tar.gz", nil
}

func attemptPrefix(jobID, leaseToken string) (string, error) {
	if !ingestionSegment.MatchString(jobID) || !ingestionSegment.MatchString(leaseToken) {
		return "", errors.New("invalid ingestion attempt identity")
	}
	return StagingPrefix + jobID + "/" + leaseToken + "/", nil
}

func (t *Tier) PresignUpload(ctx context.Context, jobID string, size int64, digest string, expiry time.Duration) (StagedUpload, error) {
	key, err := uploadKey(jobID)
	if err != nil {
		return StagedUpload{}, err
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
		return StagedUpload{}, errors.New("invalid upload SHA-256")
	}
	if size <= 0 || expiry <= 0 || expiry > 7*24*time.Hour {
		return StagedUpload{}, errors.New("upload size and expiry must be positive; expiry must not exceed seven days")
	}
	policy := minio.NewPostPolicy()
	for _, set := range []func() error{
		func() error { return policy.SetBucket(t.bucket) },
		func() error { return policy.SetKey(key) },
		func() error { return policy.SetExpires(time.Now().Add(expiry)) },
		func() error { return policy.SetContentLengthRange(size, size) },
		func() error { return policy.SetUserMetadata("sha256", digest) },
	} {
		if err := set(); err != nil {
			return StagedUpload{}, fmt.Errorf("upload policy: %w", err)
		}
	}
	url, fields, err := t.client.PresignedPostPolicy(ctx, policy)
	if err != nil {
		return StagedUpload{}, fmt.Errorf("sign upload policy: %w", err)
	}
	return StagedUpload{URL: url.String(), Fields: fields}, nil
}

func (t *Tier) StatUpload(ctx context.Context, jobID string) (StagedObject, error) {
	key, err := uploadKey(jobID)
	if err != nil {
		return StagedObject{}, err
	}
	info, err := t.client.StatObject(ctx, t.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return StagedObject{}, fmt.Errorf("stat staged upload: %w", err)
	}
	var digest string
	for k, value := range info.UserMetadata {
		if strings.EqualFold(k, "sha256") {
			digest = value
		}
	}
	return StagedObject{Key: key, Size: info.Size, SHA256: digest, ETag: info.ETag}, nil
}

func (t *Tier) OpenUpload(ctx context.Context, jobID string) (io.ReadCloser, error) {
	key, err := uploadKey(jobID)
	if err != nil {
		return nil, err
	}
	return t.client.GetObject(ctx, t.bucket, key, minio.GetObjectOptions{})
}

func (t *Tier) DeleteUpload(ctx context.Context, jobID string) error {
	key, err := uploadKey(jobID)
	if err != nil {
		return err
	}
	return t.client.RemoveObject(ctx, t.bucket, key, minio.RemoveObjectOptions{})
}

type AttemptTier struct {
	tier    *Tier
	prefix  string
	mu      sync.Mutex
	outputs map[string]Output
	sf      singleflight.Group
}

func (t *Tier) Attempt(jobID, leaseToken string) (*AttemptTier, error) {
	prefix, err := attemptPrefix(jobID, leaseToken)
	if err != nil {
		return nil, err
	}
	return &AttemptTier{tier: t, prefix: prefix, outputs: make(map[string]Output)}, nil
}

func validateArtifact(repo, side, hash string) error {
	if !ingestionSegment.MatchString(repo) || !ingestionSegment.MatchString(hash) || !slices.Contains([]string{"fe", "be", "dl"}, side) {
		return errors.New("invalid ingestion artifact identity")
	}
	return nil
}

func (t *AttemptTier) Save(ctx context.Context, repo, side, hash, srcDir string) error {
	if err := validateArtifact(repo, side, hash); err != nil {
		return err
	}
	key := t.prefix + path.Join(repo, side, hash+".tar.zst")
	_, err, _ := t.sf.Do(key, func() (any, error) {
		t.mu.Lock()
		_, saved := t.outputs[key]
		t.mu.Unlock()
		if saved {
			return struct{}{}, nil
		}
		info, err := t.tier.saveKey(ctx, key, srcDir)
		if err != nil {
			return nil, err
		}
		t.mu.Lock()
		t.outputs[key] = Output{Repo: repo, Side: side, Hash: hash, Key: key, ETag: info.ETag,
			UncompressedSize: info.UncompressedSize, FileCount: info.FileCount, CompressedSize: info.CompressedSize}
		t.mu.Unlock()
		return struct{}{}, nil
	})
	return err
}

func (t *AttemptTier) Open(ctx context.Context, repo, side, hash string) (io.ReadCloser, int64, bool, error) {
	key, err := t.readKey(repo, side, hash)
	if err != nil {
		return nil, 0, false, err
	}
	return t.tier.openKey(ctx, key)
}

func (t *AttemptTier) Stat(ctx context.Context, repo, side, hash string) (ObjectInfo, bool, error) {
	key, err := t.readKey(repo, side, hash)
	if err != nil {
		return ObjectInfo{}, false, err
	}
	return t.tier.statKey(ctx, key)
}

func (t *AttemptTier) readKey(repo, side, hash string) (string, error) {
	if err := validateArtifact(repo, side, hash); err != nil {
		return "", err
	}
	key := t.prefix + path.Join(repo, side, hash+".tar.zst")
	t.mu.Lock()
	_, saved := t.outputs[key]
	t.mu.Unlock()
	if saved {
		return key, nil
	}
	return t.tier.key(repo, side, hash), nil
}

func (t *AttemptTier) ListOutputs() []Output {
	t.mu.Lock()
	defer t.mu.Unlock()
	outputs := make([]Output, 0, len(t.outputs))
	for _, output := range t.outputs {
		outputs = append(outputs, output)
	}
	slices.SortFunc(outputs, func(a, b Output) int { return strings.Compare(a.Key, b.Key) })
	return outputs
}

func (t *Tier) Promote(ctx context.Context, output Output, overwrite bool) (bool, error) {
	if err := validateArtifact(output.Repo, output.Side, output.Hash); err != nil {
		return false, err
	}
	segments := strings.Split(output.Key, "/")
	if len(segments) != 6 || segments[0] != strings.TrimSuffix(StagingPrefix, "/") ||
		!ingestionSegment.MatchString(segments[1]) || !ingestionSegment.MatchString(segments[2]) ||
		segments[3] != output.Repo || segments[4] != output.Side || segments[5] != output.Hash+".tar.zst" || output.ETag == "" {
		return false, errors.New("invalid ingestion output key")
	}
	source, err := t.client.StatObject(ctx, t.bucket, output.Key, minio.StatObjectOptions{})
	if err != nil {
		return false, fmt.Errorf("stat ingestion output: %w", err)
	}
	if source.ETag != output.ETag || source.Size != output.CompressedSize ||
		metaInt(source.UserMetadata, metaUncompressedSize) != output.UncompressedSize ||
		metaInt(source.UserMetadata, metaFileCount) != output.FileCount {
		return false, errors.New("ingestion output changed after completion")
	}
	key := t.key(output.Repo, output.Side, output.Hash)
	if !overwrite {
		if _, err := t.client.StatObject(ctx, t.bucket, key, minio.StatObjectOptions{}); err == nil {
			return false, nil
		} else if !isNotFound(err) {
			return false, fmt.Errorf("stat canonical artifact: %w", err)
		}
	}
	_, err = t.client.CopyObject(ctx, minio.CopyDestOptions{Bucket: t.bucket, Object: key}, minio.CopySrcOptions{
		Bucket: t.bucket, Object: output.Key, MatchETag: output.ETag,
	})
	if err != nil {
		return false, fmt.Errorf("promote ingestion output: %w", err)
	}
	return true, nil
}
