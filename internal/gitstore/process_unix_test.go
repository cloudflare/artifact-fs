//go:build !windows

package gitstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

func TestRunGitCancellationClosesEscapedDescriptorPipes(t *testing.T) {
	assertEscapedGitCancellationIsBounded(t, func(ctx context.Context) error {
		_, err := runGit(ctx, "", "version")
		return err
	})
}

func TestStreamTreeRecordsCancellationClosesEscapedDescriptorPipes(t *testing.T) {
	assertEscapedGitCancellationIsBounded(t, func(ctx context.Context) error {
		return streamTreeRecords(ctx, t.TempDir(), strings.Repeat("a", 40), func(string) {})
	})
}

func TestBatchResolveSizesCancellationClosesEscapedDescriptorPipes(t *testing.T) {
	assertEscapedGitCancellationIsBounded(t, func(ctx context.Context) error {
		return New(nil).batchResolveSizes(
			ctx,
			model.RepoConfig{ID: "repo", GitDir: t.TempDir()},
			[]model.BaseNode{{ObjectOID: strings.Repeat("a", 40)}},
			[]string{strings.Repeat("a", 40)},
			map[string][]int{strings.Repeat("a", 40): {0}},
		)
	})
}

func assertEscapedGitCancellationIsBounded(t *testing.T, call func(context.Context) error) {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "escaped")
	script := "#!/bin/sh\n\"$AFS_TEST_BINARY\" -test.run '^TestRunGitEscapedDescriptorHelper$' &\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_TEST_BINARY", executable)
	t.Setenv("AFS_ESCAPED_HELPER", "1")
	t.Setenv("AFS_ESCAPED_MARKER", marker)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- call(ctx)
	}()
	deadline := time.Now().Add(time.Second)
	var helperPID int
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			helperPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("escaped helper did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if helper, err := os.FindProcess(helperPID); err == nil {
		defer helper.Kill()
	}

	started := time.Now()
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runGit error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runGit remained blocked by escaped descriptor holder")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Git cancellation took %v, want bounded descriptor wait", elapsed)
	}
}

func TestRunGitEscapedDescriptorHelper(t *testing.T) {
	if os.Getenv("AFS_ESCAPED_HELPER") != "1" {
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	marker := os.Getenv("AFS_ESCAPED_MARKER")
	markerTmp := marker + ".tmp"
	if err := os.WriteFile(markerTmp, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := os.Rename(markerTmp, marker); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	time.Sleep(10 * time.Second)
}

func TestBatchCatFileDisablesPrompts(t *testing.T) {
	repo := t.TempDir()
	run(t, "git", "init", repo)
	settings := map[string]string{
		"GIT_TERMINAL_PROMPT": "0", "GIT_ASKPASS": "", "SSH_ASKPASS": "",
		"SSH_ASKPASS_REQUIRE": "never", "GIT_SSH_COMMAND": "custom-ssh",
	}
	for key := range settings {
		t.Setenv(key, "inherited")
	}
	t.Setenv("GIT_SSH_COMMAND", "custom-ssh")
	batch, err := newBatchCatFile(filepath.Join(repo, ".git"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(batch.kill)
	for _, entry := range batch.cmd.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if want, ok := settings[key]; ok {
			if value != want {
				t.Errorf("%s = %q, want %q", key, value, want)
			}
			delete(settings, key)
		}
	}
	if len(settings) > 0 {
		t.Errorf("missing environment settings: %v", settings)
	}
	pid := batch.cmd.Process.Pid
	if sid, err := unix.Getsid(pid); err != nil || sid != pid {
		t.Errorf("batch session ID = %d, %v, want %d", sid, err, pid)
	}
}
