package supervise

// Container-mode process start: a manifest run_image runs the side's run
// command inside a stock container image with the artifact (and state dir)
// bind-mounted at their host paths — the same-path discipline the composed
// server already relies on for build containers. The published port equals
// the probed host port, so templating, health polling, and proxying are
// identical to host processes; the process must bind 0.0.0.0:{port} inside
// the container.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jmelahman/local-preview/internal/dockerapi"
)

// imagePullTimeout bounds the runtime-image pull on first use.
const imagePullTimeout = 10 * time.Minute

func (m *Manager) startContainer(k Key, p *process, spec runSpec, argv, env []string, port int, logFile *os.File) error {
	ctx, cancel := context.WithTimeout(context.Background(), imagePullTimeout)
	defer cancel()
	cli, err := m.docker(ctx)
	if err != nil {
		return fmt.Errorf("run_image %q requires a docker daemon: %w", spec.runImage, err)
	}
	if err := cli.EnsureImage(ctx, spec.runImage, logFile); err != nil {
		return err
	}

	// The per-backend deploy network pairs a process-mode frontend with its
	// backend by alias. Get-or-create on both sides keeps start order
	// irrelevant.
	networkID := ""
	alias := ""
	labels := map[string]string{
		dockerapi.ManagedLabel:  "1",
		"local-preview.repo":    p.repoName,
		"local-preview.side":    string(k.Side),
		"local-preview.hash":    k.Hash,
	}
	deployNet := ""
	switch {
	case k.Side == SideBackend:
		deployNet = networkName(p.repoName, k.Hash)
		alias = "backend"
	case k.Peer != "":
		deployNet = networkName(p.repoName, k.Peer)
		alias = "frontend"
	}
	if deployNet != "" {
		networkID, err = cli.EnsureNetwork(ctx, deployNet, map[string]string{
			dockerapi.ManagedLabel: "1",
			"local-preview.repo":   p.repoName,
		})
		if err != nil {
			return err
		}
	}

	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if cli.Rootless() {
		// Container root maps to the daemon's host user — the identity that
		// owns the artifact and state dirs.
		user = "0:0"
	}
	binds := []string{spec.dir + ":" + spec.dir}
	if spec.stateDir != "" {
		binds = append(binds, spec.stateDir+":"+spec.stateDir)
	}

	name := fmt.Sprintf("local-preview-%s-%s-%s", p.repoName, k.Side, k.Hash[:12])
	if k.Side == SideFrontend && k.Peer != "" {
		name += "-" + k.Peer[:6]
	}
	// A stale namesake from an unclean exit blocks creation; clear it. A
	// live tracked container can't collide — Key uniqueness guarantees one
	// start per identity.
	cli.RemoveContainerByName(ctx, name)

	id, err := cli.CreateContainer(ctx, dockerapi.ContainerSpec{
		Image:        spec.runImage,
		Cmd:          argv,
		Env:          append(env, "HOME=/tmp"),
		User:         user,
		WorkDir:      spec.dir,
		Binds:        binds,
		Labels:       labels,
		Name:         name,
		Port:         port,
		NetworkID:    networkID,
		NetworkAlias: alias,
	})
	if err != nil {
		return err
	}
	// External dependency networks (manifest `networks`) must already
	// exist — they belong to the user-run deps stack.
	for _, netName := range spec.networks {
		netID, ok, err := cli.FindNetworkByName(ctx, netName)
		if err == nil && !ok {
			err = fmt.Errorf("network %q not found — is the dependency stack running?", netName)
		}
		if err != nil {
			cli.RemoveContainer(ctx, id, true) //nolint:errcheck
			return fmt.Errorf("join network %q: %w", netName, err)
		}
		if err := cli.ConnectNetwork(ctx, netID, id, nil); err != nil {
			cli.RemoveContainer(ctx, id, true) //nolint:errcheck
			return fmt.Errorf("join network %q: %w", netName, err)
		}
	}
	if err := cli.StartContainer(ctx, id); err != nil {
		cli.RemoveContainer(ctx, id, true) //nolint:errcheck
		return fmt.Errorf("start container: %w", err)
	}
	p.containerID = id
	m.db.AddProcessEvent(k.RepoID, k.Hash, "start_attempt",
		fmt.Sprintf("container %.12s port %d", id, port))

	logsDone := make(chan struct{})
	go func() {
		defer close(logsDone)
		cli.StreamLogs(context.Background(), id, logFile) //nolint:errcheck
	}()

	// Reaper: the container analogue of cmd.Wait.
	go func() {
		code, waitErr := cli.WaitContainer(context.Background(), id)
		select {
		case <-logsDone:
		case <-time.After(5 * time.Second):
		}
		logFile.Close()
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		cli.RemoveContainer(rmCtx, id, true) //nolint:errcheck
		rmCancel()
		close(p.done)
		m.forget(k, p)
		if !p.intentional {
			detail := fmt.Sprintf("exit %d", code)
			if waitErr != nil {
				detail = waitErr.Error()
			}
			m.db.AddProcessEvent(k.RepoID, k.Hash, "exited", detail)
		}
	}()
	return nil
}

// runInitContainer runs one init step to completion inside the backend's
// run_image — the same mounts, user, and external dependency networks as
// the server container, so migrations reach whatever the server will. The
// image pull gets its own timeout; ctx carries the init budget.
func (m *Manager) runInitContainer(ctx context.Context, spec runSpec, argv, env []string, logFile *os.File) error {
	pullCtx, cancel := context.WithTimeout(context.Background(), imagePullTimeout)
	defer cancel()
	cli, err := m.docker(pullCtx)
	if err != nil {
		return fmt.Errorf("run_image %q requires a docker daemon: %w", spec.runImage, err)
	}
	if err := cli.EnsureImage(pullCtx, spec.runImage, logFile); err != nil {
		return err
	}
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if cli.Rootless() {
		user = "0:0"
	}
	binds := []string{spec.dir + ":" + spec.dir}
	if spec.stateDir != "" {
		binds = append(binds, spec.stateDir+":"+spec.stateDir)
	}
	return cli.RunStep(ctx, dockerapi.Step{
		Image:    spec.runImage,
		Cmd:      argv,
		WorkDir:  spec.dir,
		Env:      append(env, "HOME=/tmp"),
		User:     user,
		Binds:    binds,
		Networks: spec.networks,
	}, logFile)
}

// networkName is the per-backend deploy network shared by a backend and
// its process-mode frontend.
func networkName(repo, beHash string) string {
	return "local-preview-" + repo + "-" + beHash[:12]
}
