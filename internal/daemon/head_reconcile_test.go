package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

type headReconcileFixture struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	workDir string
	oldOID  string
	nextOID string
	lastOID string
	oldGen  int64
	svc     *Service
	rt      *repoRuntime
	logs    *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func newHeadReconcileFixture(t *testing.T) *headReconcileFixture {
	t.Helper()
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	runCmd(t, "git", "init", "--initial-branch", "main", workDir)
	readme := filepath.Join(workDir, "README.md")
	commit := func(content, message string) string {
		t.Helper()
		if err := os.WriteFile(readme, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runCmd(t, "git", "-C", workDir, "add", "README.md")
		runCmd(t, "git", "-C", workDir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", message)
		return strings.TrimSpace(runCmdOutput(t, "git", "-C", workDir, "rev-parse", "HEAD"))
	}
	oldOID := commit("old\n", "old")
	nextOID := commit("next\n", "next")
	lastOID := commit("last\n", "last")
	runCmd(t, "git", "-C", workDir, "update-ref", "refs/heads/main", oldOID)

	cfg := model.RepoConfig{
		ID:            "repro",
		Name:          "repro",
		GitDir:        filepath.Join(workDir, ".git"),
		OverlayDir:    filepath.Join(root, "overlay"),
		OverlayDBPath: filepath.Join(root, "overlay", "meta.sqlite"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(io.MultiWriter(logs), nil))
	git := gitstore.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	nodes, err := git.BuildTreeIndex(ctx, cfg, oldOID)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	snap, err := snapshot.New(ctx, filepath.Join(root, "snapshot.sqlite"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	oldGen, err := snap.PublishGeneration(ctx, oldOID, "main", nodes)
	if err != nil {
		cancel()
		_ = snap.Close()
		t.Fatal(err)
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		cancel()
		_ = snap.Close()
		t.Fatal(err)
	}
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(oldGen)
	rt := &repoRuntime{
		cfg:         cfg,
		ctx:         ctx,
		cancel:      cancel,
		snapshot:    snap,
		overlay:     ov,
		resolver:    resolver,
		headChanges: make(chan struct{}, 1),
		state:       newRuntimeState(cfg.ID, oldOID, "main", oldGen),
	}
	svc := &Service{
		logger:  logger,
		git:     git,
		running: map[model.RepoID]*repoRuntime{cfg.ID: rt},
	}
	f := &headReconcileFixture{
		t:       t,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		workDir: workDir,
		oldOID:  oldOID,
		nextOID: nextOID,
		lastOID: lastOID,
		oldGen:  oldGen,
		svc:     svc,
		rt:      rt,
		logs:    logs,
	}
	go func() {
		defer close(f.done)
		svc.headReconcileLoop(rt)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.done:
		case <-time.After(time.Second):
			t.Error("HEAD reconciliation worker did not stop")
		}
		rt.headMu.Lock()
		_ = rt.snapshot.Close()
		_ = rt.overlay.Close()
		rt.headMu.Unlock()
		git.Close()
	})
	return f
}

func (f *headReconcileFixture) updateMain(oid string) {
	f.t.Helper()
	runCmd(f.t, "git", "-C", f.workDir, "update-ref", "refs/heads/main", oid)
}

func (f *headReconcileFixture) signal() {
	f.svc.signalHEADReconcile(f.rt)
}

func (f *headReconcileFixture) waitForLog(message string) {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(f.logs.String(), message) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timed out waiting for log message %q:\n%s", message, f.logs.String())
}

func (f *headReconcileFixture) waitForHead(oid, ref string, generation int64) {
	f.t.Helper()
	waitForHeadCondition(f.t, 3*time.Second, func() bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		return f.rt.state.CurrentHEADOID == oid &&
			f.rt.state.CurrentHEADRef == ref &&
			f.rt.state.SnapshotGeneration == generation &&
			f.rt.resolver.Generation() == generation
	}, "runtime and resolver HEAD reconciliation")
}

func waitForHeadCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestHeadReconcileRetriesTransientResolveFailureWithoutAnotherSignal(t *testing.T) {
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	unavailable := f.rt.cfg.GitDir + ".unavailable"
	if err := os.Rename(f.rt.cfg.GitDir, unavailable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(unavailable); err == nil {
			_ = os.Rename(unavailable, f.rt.cfg.GitDir)
		}
	})

	f.signal()
	f.waitForLog("HEAD resolve failed")
	if err := os.Rename(unavailable, f.rt.cfg.GitDir); err != nil {
		t.Fatal(err)
	}
	f.waitForHead(f.nextOID, "main", f.oldGen+1)
}

func TestHeadReconcileRetriesTransientSnapshotPublishFailureWithoutAnotherSignal(t *testing.T) {
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	snapshotPath := filepath.Join(filepath.Dir(f.rt.cfg.OverlayDir), "snapshot.sqlite")
	if err := f.rt.snapshot.Close(); err != nil {
		t.Fatal(err)
	}

	f.signal()
	f.waitForLog("snapshot publish failed")
	recovered, err := snapshot.New(f.ctx, snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	f.rt.headMu.Lock()
	f.rt.snapshot = recovered
	f.rt.resolver.Snapshot = recovered
	f.rt.headMu.Unlock()
	f.waitForHead(f.nextOID, "main", f.oldGen+1)
}

func TestHeadReconcileRetriesTransientTreeBuildFailureWithoutAnotherSignal(t *testing.T) {
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	objectPath := filepath.Join(f.rt.cfg.GitDir, "objects", f.nextOID[:2], f.nextOID[2:])
	unavailable := objectPath + ".unavailable"
	if err := os.Rename(objectPath, unavailable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(unavailable); err == nil {
			_ = os.Rename(unavailable, objectPath)
		}
	})

	f.signal()
	f.waitForLog("tree rebuild failed")
	if err := os.Rename(unavailable, objectPath); err != nil {
		t.Fatal(err)
	}
	f.waitForHead(f.nextOID, "main", f.oldGen+1)
}

func TestHeadReconcileRetriesTransientOverlayFailureWithoutRepublishing(t *testing.T) {
	f := newHeadReconcileFixture(t)
	f.updateMain(f.nextOID)
	if err := f.rt.overlay.Close(); err != nil {
		t.Fatal(err)
	}

	f.signal()
	f.waitForLog("overlay reconcile failed")
	recovered, err := overlay.New(f.ctx, f.rt.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.rt.headMu.Lock()
	f.rt.overlay = recovered
	f.rt.resolver.Overlay = recovered
	f.rt.headMu.Unlock()
	f.waitForHead(f.nextOID, "main", f.oldGen+1)

	_, _, storedGen, err := f.rt.snapshot.ReadState(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storedGen != f.oldGen+1 {
		t.Fatalf("snapshot generation = %d, want reused generation %d", storedGen, f.oldGen+1)
	}
}

func TestHeadReconcilePersistsSameOIDRefBeforeUpdatingRuntime(t *testing.T) {
	f := newHeadReconcileFixture(t)
	runCmd(t, "git", "-C", f.workDir, "branch", "alias", f.oldOID)
	runCmd(t, "git", "-C", f.workDir, "symbolic-ref", "HEAD", "refs/heads/alias")
	snapshotPath := filepath.Join(filepath.Dir(f.rt.cfg.OverlayDir), "snapshot.sqlite")
	if err := f.rt.snapshot.Close(); err != nil {
		t.Fatal(err)
	}

	f.signal()
	f.waitForLog("snapshot head_ref update failed")
	f.svc.mu.Lock()
	gotRef := f.rt.state.CurrentHEADRef
	f.svc.mu.Unlock()
	if gotRef != "main" {
		t.Fatalf("runtime ref advanced before persistence: got %q", gotRef)
	}
	recovered, err := snapshot.New(f.ctx, snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	f.rt.headMu.Lock()
	f.rt.snapshot = recovered
	f.rt.resolver.Snapshot = recovered
	f.rt.headMu.Unlock()
	f.waitForHead(f.oldOID, "alias", f.oldGen)

	_, storedRef, _, err := recovered.ReadState(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storedRef != "alias" {
		t.Fatalf("persisted ref = %q, want alias", storedRef)
	}
}

func TestHeadReconcileCoalescesSignalsAndPublishesLatestHEAD(t *testing.T) {
	f := newHeadReconcileFixture(t)
	unavailable := f.rt.cfg.GitDir + ".unavailable"
	if err := os.Rename(f.rt.cfg.GitDir, unavailable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(unavailable); err == nil {
			_ = os.Rename(unavailable, f.rt.cfg.GitDir)
		}
	})
	runCmd(t, "git", "--git-dir", unavailable, "update-ref", "refs/heads/main", f.nextOID)
	f.signal()
	f.waitForLog("HEAD resolve failed")
	runCmd(t, "git", "--git-dir", unavailable, "update-ref", "refs/heads/main", f.lastOID)
	if err := os.Rename(unavailable, f.rt.cfg.GitDir); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		f.signal()
	}
	f.waitForHead(f.lastOID, "main", f.oldGen+1)

	_, _, storedGen, err := f.rt.snapshot.ReadState(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storedGen != f.oldGen+1 {
		t.Fatalf("snapshot generation = %d, want one coalesced publication at %d", storedGen, f.oldGen+1)
	}
}

func TestHeadReconcileBackoffStopsOnCancellation(t *testing.T) {
	f := newHeadReconcileFixture(t)
	unavailable := f.rt.cfg.GitDir + ".unavailable"
	if err := os.Rename(f.rt.cfg.GitDir, unavailable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(unavailable); err == nil {
			_ = os.Rename(unavailable, f.rt.cfg.GitDir)
		}
	})
	f.signal()
	f.waitForLog("HEAD resolve failed")

	f.cancel()
	select {
	case <-f.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("HEAD reconciliation worker did not stop during retry backoff")
	}
}
