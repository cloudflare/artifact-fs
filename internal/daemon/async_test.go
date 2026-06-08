package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func TestAddRepoAsyncRegistersWithoutClone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		Name:            "repo",
		ID:              "repo",
		RemoteURL:       "https://github.com/example/repo.git",
		Branch:          "master",
		RefreshInterval: time.Minute,
		Enabled:         true,
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStatePreparing {
		t.Fatalf("PrepareState = %q, want preparing", got.PrepareState)
	}
	if got.FetchRef != "master" {
		t.Fatalf("FetchRef = %q, want master", got.FetchRef)
	}
	if got.GitDir != filepath.Join(root, "repos", "repo", "git") {
		t.Fatalf("GitDir = %q", got.GitDir)
	}
}

func TestAddRepoAsyncRejectsInlineCredentials(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		svc.mu.Lock()
		delete(svc.running, model.RepoID("repo"))
		delete(svc.preparing, model.RepoID("repo"))
		svc.mu.Unlock()
		_ = svc.Close()
	}()

	cfg := model.RepoConfig{
		Name:      "repo",
		ID:        "repo",
		RemoteURL: "https://token@example.com/org/repo.git",
		Branch:    "master",
		Enabled:   true,
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err == nil {
		t.Fatal("expected inline credential error")
	}
}

func TestAddRepoPreparedGitDirValidation(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		Name:           "repo",
		ID:             "repo",
		Branch:         "master",
		PreparedGitDir: true,
		Enabled:        true,
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{}); err == nil {
		t.Fatal("expected --prepared-gitdir requires --async error")
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err == nil {
		t.Fatal("expected --git-dir required error")
	}
}

func TestSyncReposResetsFailedGateForRegistryRetry(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		svc.mu.Lock()
		delete(svc.running, model.RepoID("repo"))
		delete(svc.preparing, model.RepoID("repo"))
		svc.mu.Unlock()
		_ = svc.Close()
	}()

	cfg := model.RepoConfig{
		Name:      "repo",
		ID:        "repo",
		RemoteURL: "https://github.com/example/repo.git",
		Branch:    "master",
		Enabled:   true,
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err = svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}

	gate := fusefs.NewReadyGate(false)
	gate.MarkFailed(errors.New("old failure"))
	rt := &repoRuntime{
		cfg:    cfg,
		gate:   gate,
		active: false,
		state: model.RepoRuntimeState{
			RepoID:       cfg.ID,
			State:        model.PrepareStateFailed,
			PrepareError: "old failure",
		},
	}
	svc.mu.Lock()
	svc.running[cfg.ID] = rt
	svc.preparing[cfg.ID] = true // keep this unit test from starting a real worker
	svc.mu.Unlock()

	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if rt.state.State != model.PrepareStatePreparing {
		t.Fatalf("runtime state = %q, want preparing", rt.state.State)
	}
	if rt.state.PrepareError != "" {
		t.Fatalf("runtime prepare error = %q, want cleared", rt.state.PrepareError)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := gate.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gate wait = %v, want deadline after reset instead of old failure", err)
	}
}

func TestRunPrepareFailurePersistsRedactedError(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		Name:           "repo",
		ID:             "repo",
		Branch:         "master",
		GitDir:         filepath.Join(t.TempDir(), "missing.git"),
		PreparedGitDir: true,
		FetchRef:       "master",
		Enabled:        true,
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.runPrepare(ctx, got); err == nil {
		t.Fatal("expected runPrepare failure")
	}
	got, err = svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStateFailed {
		t.Fatalf("PrepareState = %q, want failed", got.PrepareState)
	}
	if got.PrepareError == "" {
		t.Fatal("PrepareError is empty, want persisted failure")
	}

	if err := svc.setPrepareState(ctx, got, model.PrepareStateFailed, errors.New("clone https://token@example.com/org/repo.git failed")); err != nil {
		t.Fatal(err)
	}
	got, err = svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.PrepareError, "token") {
		t.Fatalf("PrepareError was not redacted: %q", got.PrepareError)
	}
}

func TestRunPreparePreparedGitDirPublishesSnapshotAndMarksReady(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")

	runCmd(t, "git", "init", "--bare", bare)
	runCmd(t, "git", "clone", bare, work)
	runCmd(t, "git", "-C", work, "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, "git", "-C", work, "add", "README.md")
	runCmd(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	runCmd(t, "git", "-C", work, "push", "origin", "master")

	runCmd(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	runCmd(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "file://"+bare)

	root := filepath.Join(tmp, "artifact-fs")
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		Name:            "repo",
		ID:              "repo",
		Branch:          "master",
		RefreshInterval: time.Minute,
		GitDir:          preparedGitDir,
		PreparedGitDir:  true,
		FetchRef:        "master",
		Enabled:         true,
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.runPrepare(ctx, got); err != nil {
		t.Fatalf("runPrepare: %v", err)
	}

	got, err = svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStateReady {
		t.Fatalf("PrepareState = %q, want ready", got.PrepareState)
	}
	if got.PrepareError != "" {
		t.Fatalf("PrepareError = %q, want empty", got.PrepareError)
	}
	snap, err := snapshot.New(ctx, got.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	_, ref, gen, err := snap.ReadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "master" {
		t.Fatalf("snapshot ref = %q, want master", ref)
	}
	if gen == 0 {
		t.Fatal("snapshot generation = 0, want published generation")
	}
	if _, ok := snap.GetNode(gen, "README.md"); !ok {
		t.Fatal("README.md not found in snapshot")
	}
}

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}
