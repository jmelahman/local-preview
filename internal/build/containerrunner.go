package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/jmelahman/local-preview/internal/dockerapi"
)

// buildMount is where the scratch tree appears inside a build container.
const buildMount = "/preview-build"

// cacheMount holds per-image toolchain caches (npm's $HOME/.npm, Go's
// module/build caches) on a named volume so repeat container builds are
// warm.
const cacheMount = "/preview-cache"

// autoRunner is the default Runner: steps whose manifest side declares an
// image run in a one-shot container (when a Docker daemon is reachable);
// everything else execs on the host. The daemon is dialed lazily, once.
type autoRunner struct {
	host HostRunner

	mu     sync.Mutex
	probed bool
	client *dockerapi.Client
	err    error
}

func (r *autoRunner) docker(ctx context.Context) (*dockerapi.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.probed {
		r.probed = true
		r.client, r.err = dockerapi.Connect(ctx)
	}
	return r.client, r.err
}

// Run implements Runner.
func (r *autoRunner) Run(ctx context.Context, spec RunSpec, out io.Writer) error {
	if spec.Image == "" {
		return r.host.Run(ctx, spec, out)
	}
	cli, err := r.docker(ctx)
	if err != nil {
		fmt.Fprintf(out, "warning: build image %q requested but docker is unavailable (%v); building on the host instead\n", spec.Image, err)
		return r.host.Run(ctx, spec, out)
	}
	fmt.Fprintf(out, "[build image: %s]\n", spec.Image)

	// Under a rootless daemon container root maps to the daemon's host user
	// (the identity that owns the scratch dir); under a rootful daemon the
	// host uid:gid does. The toolchain cache volume is only mounted when
	// running as container root — a non-root user can't write a fresh
	// root-owned volume.
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	env := []string{"HOME=/tmp"}
	binds := []string{spec.ScratchDir + ":" + buildMount}
	if cli.Rootless() {
		user = "0:0"
		env = []string{
			"HOME=" + cacheMount,
			"GOMODCACHE=" + cacheMount + "/gomod",
			"GOCACHE=" + cacheMount + "/gocache",
		}
		binds = append(binds, cacheVolume(spec.Image)+":"+cacheMount)
	}
	err = cli.RunStep(ctx, dockerapi.Step{
		Image:   spec.Image,
		Cmd:     spec.Argv,
		WorkDir: path.Join(buildMount, filepath.ToSlash(spec.Dir)),
		Env:     env,
		User:    user,
		Binds:   binds,
	}, out)
	if err != nil {
		return fmt.Errorf("build step %q in %s: %w", strings.Join(spec.Argv, " "), spec.Image, err)
	}
	return nil
}

var volumeUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// cacheVolume names the per-image cache volume.
func cacheVolume(image string) string {
	return "local-preview-cache-" + volumeUnsafe.ReplaceAllString(image, "-")
}
