package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jmelahman/local-preview/internal/client"
)

func runStagedUpload(ctx context.Context, serverURL string, out io.Writer, repoName, side, name, path, ref string, overwrite, deploy, jsonOutput bool, tokenProvider func(context.Context) (string, error)) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	c := client.New(serverURL, nil).SetTokenProvider(tokenProvider)
	if ref == "" {
		sha, err := localHeadSHA()
		if err != nil {
			return fmt.Errorf("no ref given and no commit found in the current directory: %w", err)
		}
		ref = sha
	}
	if repoName == "" {
		var err error
		repoName, err = detectRepo(ctx, c)
		if err != nil {
			return err
		}
	}
	body, err := openUploadBody(path)
	if err != nil {
		return err
	}
	defer body.Close()
	archive, err := os.CreateTemp("", "preview-upload-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if _, err := io.Copy(archive, &uploadContextReader{ctx: ctx, reader: body}); err != nil {
		return fmt.Errorf("prepare staged archive: %w", err)
	}
	if err := archive.Close(); err != nil {
		return err
	}
	result, err := c.StagedUpload(ctx, repoName, side, name, ref, overwrite, deploy, archive.Name())
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(result)
	}
	target := result.Upload.Side
	if result.Upload.Name != "" {
		target += " " + result.Upload.Name
	}
	if result.Upload.Published {
		fmt.Fprintf(out, "uploaded %s for %s@%s (hash %s)\n", target, repoName, result.Upload.ShortSHA, result.Upload.Hash)
	} else {
		fmt.Fprintf(out, "%s for %s@%s already present (hash %s)\n", target, repoName, result.Upload.ShortSHA, result.Upload.Hash)
	}
	if result.Deploy != nil {
		fmt.Fprintf(out, "ready: %s\n", result.Deploy.PreviewURL)
	}
	return nil
}

type uploadContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *uploadContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
