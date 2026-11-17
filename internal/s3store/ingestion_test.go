package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

func postStagedUpload(t *testing.T, upload StagedUpload, content []byte) int {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range upload.Fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	file, err := writer.CreateFormFile("file", "input.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, upload.URL, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode
}

func TestStagedUploadPolicyAndRoundTrip(t *testing.T) {
	tier := newTestTier(t)
	ctx := t.Context()
	jobID := uuid.NewString()
	content := []byte("a staged archive")
	digest := sha256.Sum256(content)
	wantDigest := hex.EncodeToString(digest[:])
	upload, err := tier.PresignUpload(ctx, jobID, int64(len(content)), wantDigest, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tier.DeleteUpload(context.Background(), jobID) })
	policyJSON, err := base64.StdEncoding.DecodeString(upload.Fields["policy"])
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Conditions []any `json:"conditions"`
	}
	if err := json.Unmarshal(policyJSON, &policy); err != nil {
		t.Fatal(err)
	}
	var sizeBounded bool
	for _, condition := range policy.Conditions {
		if values, ok := condition.([]any); ok && len(values) == 3 && values[0] == "content-length-range" {
			sizeBounded = values[1] == float64(len(content)) && values[2] == float64(len(content))
		}
	}
	if !sizeBounded {
		t.Fatalf("policy does not bind exact archive size: %s", policyJSON)
	}
	if status := postStagedUpload(t, upload, content); status < 200 || status >= 300 {
		t.Fatalf("upload status = %d", status)
	}
	info, err := tier.StatUpload(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(content)) || info.SHA256 != wantDigest || info.ETag == "" || info.Key != StagingPrefix+jobID+"/input.tar.gz" {
		t.Fatalf("staged upload = %+v", info)
	}
	rc, err := tier.OpenUpload(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("archive = %q", got)
	}
	if err := tier.DeleteUpload(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := tier.StatUpload(ctx, jobID); !isNotFound(err) {
		t.Fatalf("deleted upload stat error = %v", err)
	}
}

func TestStagedUploadRejectsPolicyChanges(t *testing.T) {
	tier := newTestTier(t)
	content := []byte("archive")
	digest := sha256.Sum256(content)
	for _, test := range []string{"short", "long", "key", "digest"} {
		t.Run(test, func(t *testing.T) {
			jobID := uuid.NewString()
			upload, err := tier.PresignUpload(t.Context(), jobID, int64(len(content)), hex.EncodeToString(digest[:]), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tier.DeleteUpload(context.Background(), jobID) })
			body := content
			switch test {
			case "short":
				body = content[:len(content)-1]
			case "long":
				body = append(append([]byte(nil), content...), 'x')
			case "key":
				upload.Fields["key"] = StagingPrefix + uuid.NewString() + "/input.tar.gz"
			case "digest":
				upload.Fields["x-amz-meta-sha256"] = strings.Repeat("0", 64)
			}
			if status := postStagedUpload(t, upload, body); status < 400 {
				t.Fatalf("modified upload accepted: status = %d", status)
			}
		})
	}
}

func TestAttemptTierPublishesOnlyThroughPromotion(t *testing.T) {
	tier := newTestTier(t)
	ctx := t.Context()
	jobID, token := uuid.NewString(), uuid.NewString()
	attempt, err := tier.Attempt(jobID, token)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.ReplaceAll(uuid.NewString(), "-", "")
	src := writeTree(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := attempt.Save(ctx, "demo", "fe", hash, src); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	outputs := attempt.ListOutputs()
	if len(outputs) != 1 {
		t.Fatalf("outputs = %+v", outputs)
	}
	output := outputs[0]
	t.Cleanup(func() {
		_ = tier.client.RemoveObject(context.Background(), tier.bucket, output.Key, minio.RemoveObjectOptions{})
		_ = tier.Delete(context.Background(), "demo", "fe", hash)
	})
	if _, found, err := tier.Stat(ctx, "demo", "fe", hash); err != nil || found {
		t.Fatalf("canonical exists before promotion: found=%v, err=%v", found, err)
	}
	if info, found, err := attempt.Stat(ctx, "demo", "fe", hash); err != nil || !found || info.ETag != output.ETag {
		t.Fatalf("attempt stat = %+v, found=%v, err=%v", info, found, err)
	}
	rc, want, found, err := attempt.Open(ctx, "demo", "fe", hash)
	if err != nil || !found || want != output.UncompressedSize {
		t.Fatalf("attempt open: want=%d, found=%v, err=%v", want, found, err)
	}
	if err := extractTarTest(t, rc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.Key, StagingPrefix+jobID+"/"+token+"/") {
		t.Fatalf("output escaped attempt prefix: %q", output.Key)
	}
	if published, err := tier.Promote(ctx, output, false); err != nil || !published {
		t.Fatalf("promote: published=%v, err=%v", published, err)
	}
	if published, err := tier.Promote(ctx, output, false); err != nil || published {
		t.Fatalf("repeat promote: published=%v, err=%v", published, err)
	}
	if _, found, err := tier.Stat(ctx, "demo", "fe", hash); err != nil || !found {
		t.Fatalf("canonical missing after promotion: found=%v, err=%v", found, err)
	}
	second, err := tier.Attempt(jobID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := second.Save(ctx, "demo", "fe", hash, src); err != nil {
		t.Fatal(err)
	}
	replacement := second.ListOutputs()[0]
	t.Cleanup(func() {
		_ = tier.client.RemoveObject(context.Background(), tier.bucket, replacement.Key, minio.RemoveObjectOptions{})
	})
	if published, err := tier.Promote(ctx, replacement, false); err != nil || published {
		t.Fatalf("skip existing: published=%v, err=%v", published, err)
	}
	if published, err := tier.Promote(ctx, replacement, true); err != nil || !published {
		t.Fatalf("overwrite: published=%v, err=%v", published, err)
	}
	rc, _, found, err = tier.Open(ctx, "demo", "fe", hash)
	if err != nil || !found {
		t.Fatalf("open replacement: found=%v, err=%v", found, err)
	}
	dst := t.TempDir()
	if err := extractTarTest(t, rc, dst); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	got, err := os.ReadFile(filepath.Join(dst, "index.html"))
	if err != nil || string(got) != "replacement" {
		t.Fatalf("replaced content = %q, err=%v", got, err)
	}
}

func TestPromoteRejectsChangedObject(t *testing.T) {
	tier := newTestTier(t)
	attempt, err := tier.Attempt(uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.Save(t.Context(), "demo", "fe", "changed", writeTree(t)); err != nil {
		t.Fatal(err)
	}
	output := attempt.ListOutputs()[0]
	t.Cleanup(func() {
		_ = tier.client.RemoveObject(context.Background(), tier.bucket, output.Key, minio.RemoveObjectOptions{})
	})
	if _, err := tier.client.PutObject(t.Context(), tier.bucket, output.Key, strings.NewReader("changed"), 7, minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if published, err := tier.Promote(t.Context(), output, true); err == nil || published {
		t.Fatalf("stale output accepted: published=%v, err=%v", published, err)
	}
}

func TestIngestionRejectsUnsafeIdentities(t *testing.T) {
	tier := &Tier{}
	for _, id := range []string{"", "../escape", "one/two", ".", "a.b"} {
		if _, err := tier.Attempt(id, "safe"); err == nil {
			t.Errorf("unsafe job ID accepted: %q", id)
		}
		if _, err := tier.Attempt("safe", id); err == nil {
			t.Errorf("unsafe lease token accepted: %q", id)
		}
		if _, err := tier.PresignUpload(t.Context(), id, 1, strings.Repeat("0", 64), time.Minute); err == nil {
			t.Errorf("unsafe upload ID accepted: %q", id)
		}
	}
	for _, digest := range []string{"", "short", strings.Repeat("G", 64), strings.Repeat("A", 64)} {
		if _, err := tier.PresignUpload(t.Context(), "safe", 1, digest, time.Minute); err == nil {
			t.Errorf("invalid digest accepted: %q", digest)
		}
	}
}
