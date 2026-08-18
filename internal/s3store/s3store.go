// Package s3store is the durable artifact tier: a content-addressed object
// store (S3 or any S3-compatible endpoint such as MinIO) that outlives the
// local disk cache. Build artifacts are Saved here right after they publish
// locally; when retention later evicts the local copy, the object persists, so
// a rebuild Opens it and skips the build instead of re-running every step.
//
// Objects are keyed by the same content-address the local store uses, so the
// tier is a trivial immutable key→blob map:
//
//	<prefix>/<repo>/<side>/<hash>.tar.zst     side ∈ {fe, be, dl}
//
// The codec is explicit in the suffix and chosen at read time, so the on-disk
// format stays additive. A truncated blob must never land under a
// content-addressed key (Save's skip-if-exists would make it permanent), so an
// upload is compressed to a temp file first — a walk/tar failure aborts before
// any bytes are put — and Open verifies the decompressed length against
// metadata recorded at Save time.
package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// User-metadata keys carrying the uncompressed tar's byte size and file count.
// Size is verified on Open; both give a future reconcile pass an integrity
// check beyond "the object exists".
const (
	metaUncompressedSize = "uncompressed-size"
	metaFileCount        = "file-count"
)

// ErrSourceGone reports that the source directory (or a file within it)
// vanished mid-walk — retention's GC deleted the just-built artifact out from
// under a persist. Callers treat it as a benign drop, not a failure to retry.
var ErrSourceGone = errors.New("artifact source directory disappeared during upload")

// Config configures the object-store connection. Endpoint and Bucket are
// required; everything else is optional.
type Config struct {
	Endpoint  string // host:port, e.g. s3.amazonaws.com or minio:9000
	Bucket    string
	Prefix    string // optional key prefix within the bucket
	Region    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	// TmpDir is where an upload is compressed before it's put. Empty uses the
	// OS default; set it to the store's tmp dir so staging shares the
	// artifacts filesystem.
	TmpDir string
}

// Tier is a durable content-addressed artifact store.
type Tier struct {
	client *minio.Client
	bucket string
	prefix string
	tmpDir string
}

// New dials the endpoint and verifies the bucket exists. It fails closed: a
// misconfigured or unreachable bucket returns an error rather than a Tier that
// would silently drop every artifact.
func New(cfg Config) (*Tier, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 tier: endpoint and bucket are required")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 tier: %w", err)
	}
	ok, err := client.BucketExists(context.Background(), cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3 tier: check bucket %q: %w", cfg.Bucket, err)
	}
	if !ok {
		return nil, fmt.Errorf("s3 tier: bucket %q does not exist", cfg.Bucket)
	}
	return &Tier{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix, tmpDir: cfg.TmpDir}, nil
}

// key is the object name for a content-addressed artifact side.
func (t *Tier) key(repo, side, hash string) string {
	return path.Join(t.prefix, repo, side, hash+".tar.zst")
}

// Save tars srcDir's contents, zstd-compresses them, and uploads the result
// under the content-addressed key. It is a no-op if the object already exists
// (content-addressing makes an identical key byte-identical). It returns
// ErrSourceGone if the source directory disappears mid-walk (a GC race).
func (t *Tier) Save(ctx context.Context, repo, side, hash, srcDir string) error {
	key := t.key(repo, side, hash)
	if _, err := t.client.StatObject(ctx, t.bucket, key, minio.StatObjectOptions{}); err == nil {
		return nil // already present
	} else if !isNotFound(err) {
		return fmt.Errorf("s3 tier: stat %s: %w", key, err)
	}

	// Two-pass: compress to a temp file so the object's size and integrity
	// metadata are known before the put, and a walk/tar failure aborts before
	// any bytes reach the bucket.
	tmp, err := os.CreateTemp(t.tmpDir, "s3-artifact-*.tar.zst")
	if err != nil {
		return fmt.Errorf("s3 tier: stage upload: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	zw, err := zstd.NewWriter(tmp)
	if err != nil {
		return fmt.Errorf("s3 tier: zstd: %w", err)
	}
	size, count, err := tarDir(zw, srcDir)
	if err != nil {
		zw.Close()
		return err // includes ErrSourceGone, propagated verbatim
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("s3 tier: finish compression: %w", err)
	}
	compressed, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("s3 tier: size staged upload: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("s3 tier: rewind staged upload: %w", err)
	}

	_, err = t.client.PutObject(ctx, t.bucket, key, tmp, compressed, minio.PutObjectOptions{
		ContentType: "application/zstd",
		UserMetadata: map[string]string{
			metaUncompressedSize: strconv.FormatInt(size, 10),
			metaFileCount:        strconv.FormatInt(count, 10),
		},
	})
	if err != nil {
		return fmt.Errorf("s3 tier: put %s: %w", key, err)
	}
	return nil
}

// Open returns a reader over the artifact's decompressed tar bytes, or
// found=false (not an error) when the object is absent. The returned
// ReadCloser's Close verifies the decompressed length matches the size
// recorded at Save time — a mismatch is returned as an error so the caller
// discards the extraction and rebuilds. The caller is responsible for the
// hardened filesystem extraction of the tar stream.
func (t *Tier) Open(ctx context.Context, repo, side, hash string) (io.ReadCloser, bool, error) {
	key := t.key(repo, side, hash)
	info, err := t.client.StatObject(ctx, t.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("s3 tier: stat %s: %w", key, err)
	}
	obj, err := t.client.GetObject(ctx, t.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("s3 tier: get %s: %w", key, err)
	}
	zr, err := zstd.NewReader(obj)
	if err != nil {
		obj.Close()
		return nil, false, fmt.Errorf("s3 tier: zstd %s: %w", key, err)
	}
	return &verifyReadCloser{zr: zr, obj: obj, want: metaInt(info.UserMetadata, metaUncompressedSize)}, true, nil
}

// verifyReadCloser exposes the decompressed tar stream and, on Close, checks
// that the number of bytes read matches the expected uncompressed size.
type verifyReadCloser struct {
	zr   *zstd.Decoder
	obj  *minio.Object
	want int64
	read int64
}

func (v *verifyReadCloser) Read(p []byte) (int, error) {
	n, err := v.zr.Read(p)
	v.read += int64(n)
	return n, err
}

func (v *verifyReadCloser) Close() error {
	v.zr.Close()
	err := v.obj.Close()
	if v.want > 0 && v.read != v.want {
		return fmt.Errorf("s3 tier: integrity check failed: decompressed %d bytes, expected %d", v.read, v.want)
	}
	return err
}

// isNotFound reports whether err is S3's "no such key" (a genuinely absent
// object) rather than a transport or auth failure.
func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode == http.StatusNotFound || resp.Code == "NoSuchKey"
}

// metaInt reads an integer user-metadata value case-insensitively (minio
// canonicalizes header keys, so the exact casing round-tripped isn't
// guaranteed). Returns 0 when absent or unparseable.
func metaInt(md map[string]string, key string) int64 {
	for k, v := range md {
		if strings.EqualFold(k, key) {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}
