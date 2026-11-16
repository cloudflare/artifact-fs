//go:build !windows

package gitstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
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

// git read-tree holds index.lock while it runs the fsmonitor hook. A canceled
// read-tree that gets SIGKILL leaves the lock, and every later command that
// writes the index fails with "index.lock: File exists". On SIGTERM git
// removes it.
func TestCanceledPrepareFetchedBranchLeavesNoIndexLock(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	for _, name := range []string{"README.md", "ADVANCED.md"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", repo, "add", name)
		run(t, "git", "-C", repo, "commit", "-m", name)
	}
	advanced := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "update-ref", "refs/remotes/origin/main", advanced)
	run(t, "git", "-C", repo, "reset", "--hard", "HEAD~1")
	gitDir := filepath.Join(repo, ".git")

	// The hook hangs once while index.lock exists, until the cancel.
	lock := filepath.Join(gitDir, "index.lock")
	blocked := filepath.Join(tmp, "read-tree-blocked")
	hook := filepath.Join(tmp, "fsmonitor-hook")
	script := fmt.Sprintf("#!/bin/sh\nif [ -e %[1]s ] && [ ! -e %[2]s ]; then\n  : > %[2]s\n  sleep 60\nfi\nexit 1\n", shellScriptQuote(lock), shellScriptQuote(blocked))
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "config", "core.fsmonitor", hook)

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: gitDir}
	store := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- store.PrepareFetchedBranch(ctx, cfg, "main") }()
	for {
		if _, err := os.Stat(blocked); err == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("PrepareFetchedBranch returned before read-tree ran the hook: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("PrepareFetchedBranch error = %v, want context canceled", err)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Error("the canceled read-tree left index.lock behind")
	}
	if err := store.PrepareFetchedBranch(context.Background(), cfg, "main"); err != nil {
		t.Fatalf("the next PrepareFetchedBranch: %v", err)
	}
	if head := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD")); head != advanced {
		t.Fatalf("HEAD = %s, want %s", head, advanced)
	}
}

// A descendant that ignores SIGTERM still dies: the SIGKILL that follows the
// SIGTERM reaches the whole process group.
func TestRunGitCancellationKillsADescendantThatIgnoresSIGTERM(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(tmp, "descendant-pid")
	survived := filepath.Join(tmp, "survived")
	descendant := fmt.Sprintf("trap '' TERM; echo $$ > %[1]s.tmp; mv %[1]s.tmp %[1]s; sleep 60; : > %[2]s", shellScriptQuote(pidFile), shellScriptQuote(survived))
	script := fmt.Sprintf("#!/bin/sh\nsh -c %s &\nwait\n", shellScriptQuote(descendant))
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := runGit(ctx, "", "version")
		errCh <- err
	}()
	var pid int
	for {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
				t.Fatal(err)
			}
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("runGit returned before its descendant started: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("runGit error = %v, want context canceled", err)
	}
	// The SIGKILL follows gitCommandWaitDelay after the SIGTERM.
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("descendant %d that ignores SIGTERM survived the cancellation", pid)
		}
	}
	if _, err := os.Stat(survived); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the descendant ran to its end")
	}
}
