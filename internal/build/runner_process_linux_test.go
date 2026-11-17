//go:build linux

package build

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHostRunnerCancellationStopsDescendants(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (HostRunner{}).Run(ctx, RunSpec{
			ScratchDir: dir,
			Argv:       []string{"sh", "-c", `sh -c 'echo $$ > "$1"; exec sleep 30' child "$1" & wait`, "parent", pidPath},
		}, io.Discard)
	}()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidPath); err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("build did not start its child")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled build succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("build did not return after cancellation")
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		state := strings.Fields(string(stat)[bytes.LastIndexByte(stat, ')')+1:])[0]
		if state == "Z" || state == "X" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("build child kept running after cancellation")
}
