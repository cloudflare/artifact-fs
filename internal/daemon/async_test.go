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

func TestSyncReposSkipsResetWhilePrepareWorkerInFlight(t *testing.T) {
	ctx := context.Background()
	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		svc.mu.Lock()
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

	gate := fusefs.NewReadyGate(true)
	rt := &repoRuntime{
		cfg:    cfg,
		gate:   gate,
		active: true,
		state: model.RepoRuntimeState{
			RepoID:       cfg.ID,
			State:        repoStateMounted,
			PrepareError: "",
		},
	}
	svc.mu.Lock()
	svc.running[cfg.ID] = rt
	svc.preparing[cfg.ID] = 1
	svc.mu.Unlock()

	if err := svc.syncRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if rt.state.State != repoStateMounted {
		t.Fatalf("runtime state = %q, want mounted", rt.state.State)
	}
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("gate was reset while prepare worker was in flight: %v", err)
	}
}

func TestRestartRunningPrepareSkipsStalePreparingSnapshot(t *testing.T) {
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
	latest, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.registry.UpdatePrepareState(ctx, latest.ID, model.PrepareStateReady, ""); err != nil {
		t.Fatal(err)
	}
	stale := latest
	stale.PrepareState = model.PrepareStatePreparing

	gate := fusefs.NewReadyGate(true)
	rt := &repoRuntime{
		cfg:    latest,
		gate:   gate,
		active: true,
		state: model.RepoRuntimeState{
			RepoID: latest.ID,
			State:  repoStateMounted,
		},
	}
	svc.mu.Lock()
	svc.running[latest.ID] = rt
	svc.mu.Unlock()

	svc.restartRunningPrepareIfCurrent(ctx, stale, rt, false)
	if rt.state.State != repoStateMounted {
		t.Fatalf("runtime state = %q, want mounted", rt.state.State)
	}
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("gate was reset from stale registry snapshot: %v", err)
	}
	svc.mu.Lock()
	_, preparing := svc.preparing[latest.ID]
	svc.mu.Unlock()
	if preparing {
		t.Fatal("started prepare worker from stale registry snapshot")
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

func TestStartPrepareWorkerTimesOutAndPersistsFailed(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitPath := filepath.Join(bin, "git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	svc, err := New(ctx, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	svc.prepareTimeout = 20 * time.Millisecond
	defer svc.Close()

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
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	svc.startPrepareWorker(ctx, got)

	got = waitForPrepareState(t, svc, "repo", model.PrepareStateFailed)
	if got.PrepareError != "prepare timed out" {
		t.Fatalf("PrepareError = %q, want timeout", got.PrepareError)
	}
	waitFor(t, time.Second, func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		_, preparing := svc.preparing[got.ID]
		return !preparing
	})
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

func TestRunPrepareDoesNotOpenGateWhenReadyPersistenceFails(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	preparedGitDir := createPreparedGitDir(t, tmp)

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if err := svc.mountAsyncRepo(ctx, got); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	gate := svc.running[got.ID].gate
	svc.mu.Unlock()
	if gate == nil {
		t.Fatal("runtime gate is nil")
	}
	if err := svc.registry.Close(); err != nil {
		t.Fatal(err)
	}

	if err := svc.runPrepare(ctx, got); err == nil {
		t.Fatal("expected ready state persistence failure")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := gate.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gate wait = %v, want deadline because ready was not durably persisted", err)
	}
}

func TestRunPrepareDoesNotMarkSupersededConfigReady(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	firstGitDir := createPreparedGitDir(t, filepath.Join(tmp, "first"))
	secondGitDir := createPreparedGitDir(t, filepath.Join(tmp, "second"))

	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		Name:            "repo",
		ID:              "repo",
		Branch:          "master",
		RefreshInterval: time.Minute,
		GitDir:          firstGitDir,
		PreparedGitDir:  true,
		FetchRef:        "master",
		Enabled:         true,
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	cfg.GitDir = secondGitDir
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}

	err = svc.runPrepare(ctx, first)
	if err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Fatalf("runPrepare error = %v, want superseded", err)
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStatePreparing {
		t.Fatalf("PrepareState = %q, want preparing", got.PrepareState)
	}
	if got.GitDir != secondGitDir {
		t.Fatalf("GitDir = %q, want newer git dir %q", got.GitDir, secondGitDir)
	}
}

func TestRunPrepareDoesNotMarkSupersededConfigFailed(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	svc, err := New(ctx, filepath.Join(tmp, "artifact-fs"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	cfg := model.RepoConfig{
		Name:            "repo",
		ID:              "repo",
		Branch:          "master",
		RefreshInterval: time.Minute,
		GitDir:          filepath.Join(tmp, "missing.git"),
		PreparedGitDir:  true,
		FetchRef:        "master",
		Enabled:         true,
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}
	first, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	cfg.GitDir = createPreparedGitDir(t, filepath.Join(tmp, "second"))
	if err := svc.AddRepoWithOptions(ctx, cfg, AddRepoOptions{Async: true}); err != nil {
		t.Fatal(err)
	}

	if err := svc.runPrepare(ctx, first); err == nil {
		t.Fatal("expected stale prepare failure")
	}
	got, err := svc.registry.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.PrepareState != model.PrepareStatePreparing {
		t.Fatalf("PrepareState = %q, want preparing", got.PrepareState)
	}
	if got.PrepareError != "" {
		t.Fatalf("PrepareError = %q, want empty", got.PrepareError)
	}
}

func createPreparedGitDir(t *testing.T, tmp string) string {
	t.Helper()
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
	return preparedGitDir
}

func waitForPrepareState(t *testing.T, svc *Service, name string, state string) model.RepoConfig {
	t.Helper()
	var got model.RepoConfig
	waitFor(t, 2*time.Second, func() bool {
		var err error
		got, err = svc.registry.GetRepo(context.Background(), name)
		return err == nil && got.PrepareState == state
	})
	return got
}

func waitFor(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func runCmd(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}
