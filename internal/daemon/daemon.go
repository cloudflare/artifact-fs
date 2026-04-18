package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/cloudflare/artifact-fs/internal/watcher"
)

const DefaultHydrationConcurrency = 4

const (
	repoStateMounted   = "mounted"
	repoStateUnmounted = "unmounted"
	repoStateDegraded  = "degraded"
)

type Service struct {
	root                 string
	mountRoot            string
	hydrationConcurrency int
	logger               *slog.Logger
	registry             *registry.Store
	git                  *gitstore.Store
	mu                   sync.Mutex
	running              map[model.RepoID]*repoRuntime
	preparing            map[model.RepoID]bool
	mountFailures        map[model.RepoID]*mountFailure
}

type mountFailure struct {
	lastAttempt time.Time
	backoff     time.Duration
}

type repoRuntime struct {
	cfg      model.RepoConfig
	ctx      context.Context
	cancel   context.CancelFunc
	snapshot *snapshot.Store
	overlay  *overlay.Store
	hydrator *hydrator.Service
	resolver *fusefs.Resolver
	mfs      fusefs.MountedFS
	gate     *fusefs.ReadyGate
	state    model.RepoRuntimeState
	active   bool
}

type aheadBehind struct {
	ahead    int
	behind   int
	diverged bool
}

type AddRepoOptions struct {
	Async bool
}

func New(ctx context.Context, root string, logger *slog.Logger) (*Service, error) {
	reg, err := registry.New(ctx, filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		return nil, err
	}
	svc := &Service{
		root:          root,
		logger:        logger,
		registry:      reg,
		git:           gitstore.New(logger),
		running:       map[model.RepoID]*repoRuntime{},
		preparing:     map[model.RepoID]bool{},
		mountFailures: map[model.RepoID]*mountFailure{},
	}
	svc.git.SetBatchPoolSize(DefaultHydrationConcurrency)
	return svc, nil
}

func (s *Service) SetMountRoot(root string) {
	if strings.TrimSpace(root) != "" {
		s.mountRoot = root
	}
}

func (s *Service) SetHydrationConcurrency(n int) {
	if n > 0 {
		s.hydrationConcurrency = n
		s.git.SetBatchPoolSize(n)
	}
}

func (s *Service) hydrationWorkers() int {
	if s.hydrationConcurrency > 0 {
		return s.hydrationConcurrency
	}
	return DefaultHydrationConcurrency
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rt := range s.running {
		s.stopRuntime(rt)
		delete(s.running, id)
	}
	s.git.Close()
	return s.registry.Close()
}

func (s *Service) Start(ctx context.Context) error {
	// Initial mount of all registered repos.
	if err := s.syncRepos(ctx); err != nil {
		return err
	}

	// Poll the registry for repos added or removed after startup.
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.syncRepos(ctx); err != nil {
				s.logger.Error("registry sync failed", "error", err)
			}
		}
	}
}

// syncRepos reconciles the running set with the registry. Mounts new repos
// and unmounts repos that were removed or disabled.
func (s *Service) syncRepos(ctx context.Context) error {
	repos, err := s.registry.ListRepos(ctx)
	if err != nil {
		return err
	}

	registered := map[model.RepoID]bool{}
	for _, repo := range repos {
		registered[repo.ID] = true
		if !repo.Enabled {
			s.unmount(repo.ID)
			continue
		}
		s.mu.Lock()
		rt, running := s.running[repo.ID]
		s.mu.Unlock()
		if running {
			if repo.PrepareState == model.PrepareStatePreparing && rt != nil && !rt.active {
				s.startPrepareWorker(ctx, repo)
			}
			continue
		}
		if shouldMountAsync(repo) {
			if err := s.mountAsyncRepo(ctx, repo); err != nil {
				s.logger.Error("repo async mount failed", "repo", repo.Name, "error", err)
				continue
			}
			if repo.PrepareState == model.PrepareStatePreparing {
				s.startPrepareWorker(ctx, repo)
			}
			continue
		}
		if mf, ok := s.mountFailures[repo.ID]; ok && time.Since(mf.lastAttempt) < mf.backoff {
			continue
		}
		s.logger.Info("mounting repo", "repo", repo.Name)
		if err := s.mountRepo(ctx, repo); err != nil {
			s.logger.Error("repo mount failed", "repo", repo.Name, "error", err)
			mf := s.mountFailures[repo.ID]
			if mf == nil {
				mf = &mountFailure{}
				s.mountFailures[repo.ID] = mf
			}
			mf.lastAttempt = time.Now()
			if mf.backoff == 0 {
				mf.backoff = 30 * time.Second
			} else {
				mf.backoff = min(mf.backoff*2, 5*time.Minute)
			}
		} else {
			delete(s.mountFailures, repo.ID)
		}
	}

	// Unmount repos that were removed from the registry.
	s.mu.Lock()
	var stale []model.RepoID
	for id := range s.running {
		if !registered[id] {
			stale = append(stale, id)
		}
	}
	s.mu.Unlock()
	for _, id := range stale {
		s.logger.Info("unmounting removed repo", "repo", id)
		s.unmount(id)
		delete(s.mountFailures, id)
	}
	for id := range s.mountFailures {
		if !registered[id] {
			delete(s.mountFailures, id)
		}
	}

	return nil
}

func (s *Service) AddRepo(ctx context.Context, cfg model.RepoConfig) error {
	return s.AddRepoWithOptions(ctx, cfg, AddRepoOptions{})
}

func (s *Service) AddRepoWithOptions(ctx context.Context, cfg model.RepoConfig, opts AddRepoOptions) error {
	if err := model.ValidateRepoName(cfg.Name); err != nil {
		return err
	}
	if cfg.ID == "" {
		cfg.ID = model.RepoID(cfg.Name)
	}
	cfg.RemoteURLRedacted = auth.RedactRemoteURL(cfg.RemoteURL)
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 30 * time.Second
	}
	s.fillPaths(&cfg)
	if strings.TrimSpace(cfg.FetchRef) == "" {
		cfg.FetchRef = cfg.Branch
	}
	cloneURL := cfg.RemoteURL
	if opts.Async {
		if strings.TrimSpace(cfg.RemoteURL) == "" && !cfg.PreparedGitDir {
			return fmt.Errorf("--remote is required unless --prepared-gitdir is set")
		}
		if auth.HasInlineCredentials(cfg.RemoteURL) {
			return fmt.Errorf("async repositories must use ambient credentials; remove credentials from --remote")
		}
		cfg.PrepareState = model.PrepareStatePreparing
		cfg.PrepareError = ""
	} else if auth.HasInlineCredentials(cfg.RemoteURL) {
		cfg.RemoteURL = ""
	}
	if err := s.registry.AddRepo(ctx, cfg); err != nil {
		return err
	}
	if opts.Async {
		return nil
	}
	// Clone and build snapshot so the repo is ready to mount, but don't start
	// the FUSE server -- that's the daemon's job.
	cfg.RemoteURL = cloneURL
	return s.prepareRepo(ctx, cfg)
}

func (s *Service) RemoveRepo(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	s.unmount(cfg.ID)
	return s.registry.RemoveRepo(ctx, name)
}

func (s *Service) ListRepos(ctx context.Context) ([]model.RepoConfig, error) {
	return s.registry.ListRepos(ctx)
}

func (s *Service) SetRefresh(ctx context.Context, name string, interval time.Duration) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	cfg.RefreshInterval = interval
	if err := s.registry.AddRepo(ctx, cfg); err != nil {
		return err
	}
	s.mu.Lock()
	if rt, ok := s.running[cfg.ID]; ok {
		rt.cfg.RefreshInterval = interval
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) Status(ctx context.Context, name string) (model.RepoRuntimeState, error) {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return model.RepoRuntimeState{}, err
	}

	// If we're the running daemon, use in-memory state.
	s.mu.Lock()
	rt, ok := s.running[cfg.ID]
	if ok {
		dirty, _ := rt.overlay.DirtyCount(ctx)
		rt.state.DirtyOverlay = dirty > 0
		st := rt.state // copy under lock
		cfg = rt.cfg
		s.mu.Unlock()
		applyHydrationStats(&st, cfg.BlobCacheDir)
		return st, nil
	}
	s.mu.Unlock()
	return s.readPersistedStatus(ctx, cfg), nil
}

func (s *Service) FetchNow(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if cfg.PrepareState == model.PrepareStatePreparing {
		return fusefs.ErrRepoNotReady
	}
	if cfg.PrepareState == model.PrepareStateFailed {
		return fmt.Errorf("repo prepare failed: %s", cfg.PrepareError)
	}
	if err := s.git.Fetch(ctx, cfg); err != nil {
		return err
	}
	state, err := s.fetchState(ctx, cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if rt, ok := s.running[cfg.ID]; ok {
		markFetchSuccess(&rt.state, time.Now(), state)
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) Prepare(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if !isAsyncRepo(cfg) {
		return s.prepareRepo(ctx, cfg)
	}
	if cfg.PrepareState == model.PrepareStateReady {
		return nil
	}
	cfg.PrepareState = model.PrepareStatePreparing
	cfg.PrepareError = ""
	if strings.TrimSpace(cfg.FetchRef) == "" {
		cfg.FetchRef = cfg.Branch
	}
	if err := s.registry.AddRepo(ctx, cfg); err != nil {
		return err
	}
	var running bool
	s.mu.Lock()
	if rt, ok := s.running[cfg.ID]; ok && rt.gate != nil {
		running = true
		rt.gate.Reset()
		rt.cfg = cfg
		rt.state.State = model.PrepareStatePreparing
		rt.state.PrepareError = ""
	}
	s.mu.Unlock()
	if running {
		s.startPrepareWorker(ctx, cfg)
	}
	return nil
}

func (s *Service) Remount(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	s.unmount(cfg.ID)
	if shouldMountAsync(cfg) {
		if err := s.mountAsyncRepo(ctx, cfg); err != nil {
			return err
		}
		if cfg.PrepareState == model.PrepareStatePreparing {
			s.startPrepareWorker(ctx, cfg)
		}
		return nil
	}
	return s.mountRepo(ctx, cfg)
}

func (s *Service) Unmount(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	s.unmount(cfg.ID)
	return nil
}

// prepareRepo clones the git repo and builds the initial snapshot. It does NOT
// start a FUSE mount or any background goroutines, so it's safe to call from
// short-lived CLI commands like add-repo.
func (s *Service) prepareRepo(ctx context.Context, cfg model.RepoConfig) error {
	snap, _, _, _, err := s.ensurePreparedRepo(ctx, cfg)
	if err != nil {
		return err
	}
	defer snap.Close()
	return nil
}

// ensurePreparedRepo makes sure the repo is cloned and has an initial snapshot.
// The returned snapshot store remains open for callers that need to continue
// into runtime startup.
func (s *Service) ensurePreparedRepo(ctx context.Context, cfg model.RepoConfig) (*snapshot.Store, string, string, int64, error) {
	if err := os.MkdirAll(cfg.MountPath, 0o755); err != nil {
		return nil, "", "", 0, err
	}
	if err := s.git.CloneBlobless(ctx, cfg); err != nil {
		return nil, "", "", 0, err
	}
	headOID, headRef, err := s.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		return nil, "", "", 0, err
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		return nil, "", "", 0, err
	}
	storedOID, storedRef, gen, err := snap.ReadState(ctx)
	if err != nil || gen == 0 || storedOID != headOID || storedRef != headRef {
		gen, _, err = s.publishSnapshot(ctx, cfg, snap, headOID, headRef)
		if err != nil {
			snap.Close()
			return nil, "", "", 0, err
		}
	}
	return snap, headOID, headRef, gen, nil
}

// mountRepo opens all stores, starts the FUSE server, watcher, and refresh
// loop. Called by the daemon's Start for each registered repo.
func (s *Service) mountRepo(ctx context.Context, cfg model.RepoConfig) error {
	snap, headOID, headRef, gen, err := s.ensurePreparedRepo(ctx, cfg)
	if err != nil {
		return err
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		snap.Close()
		return err
	}
	baseLookup := func(path string) (model.BaseNode, bool) {
		return snap.GetNode(gen, path)
	}
	if err := ov.Reconcile(ctx, baseLookup); err != nil {
		ov.Close()
		snap.Close()
		return err
	}
	if err := s.git.ReadTreeHEAD(ctx, cfg); err != nil {
		ov.Close()
		snap.Close()
		return err
	}
	h := hydrator.New(s.git)

	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	s.refreshCommitTime(ctx, cfg, headOID, resolver, "commit timestamp unavailable, mtime will use generation fallback")

	h.SetOnHydrated(func(_ model.RepoID, objectOID string, size int64) {
		snap.UpdateSize(resolver.Generation(), objectOID, size)
	})
	h.Start(s.hydrationWorkers(), cfg)
	engine := &fusefs.Engine{
		Resolver: resolver,
		Repo:     cfg,
		Overlay:  ov,
		Hydrator: h,
	}

	mfs, err := fusefs.MountRepo(cfg, resolver, engine)
	if err != nil {
		s.logger.Error("fuse mount failed, running without FUSE", "repo", cfg.Name, "error", err)
		mfs = nil
	}
	runtimeCtx, cancel := context.WithCancel(ctx)

	rt := &repoRuntime{
		cfg:      cfg,
		ctx:      runtimeCtx,
		cancel:   cancel,
		snapshot: snap,
		overlay:  ov,
		hydrator: h,
		resolver: resolver,
		mfs:      mfs,
		state:    newRuntimeState(cfg.ID, headOID, headRef, gen),
	}
	s.startRuntime(rt)
	s.startRepoBackground(rt)

	return nil
}

func (s *Service) mountAsyncRepo(ctx context.Context, cfg model.RepoConfig) error {
	s.fillPaths(&cfg)
	if err := os.MkdirAll(cfg.MountPath, 0o755); err != nil {
		return err
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		return err
	}
	headOID, headRef, gen, _ := snap.ReadState(ctx)
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		snap.Close()
		return err
	}
	h := hydrator.New(s.git)
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	if headOID != "" {
		s.refreshCommitTime(ctx, cfg, headOID, resolver, "commit timestamp unavailable, mtime will use generation fallback")
	}
	h.SetOnHydrated(func(_ model.RepoID, objectOID string, size int64) {
		snap.UpdateSize(resolver.Generation(), objectOID, size)
	})
	h.Start(s.hydrationWorkers(), cfg)

	gate := fusefs.NewReadyGate(false)
	if cfg.PrepareState == model.PrepareStateFailed {
		gate.MarkFailed(prepareGateError(cfg.PrepareError))
	}
	engine := &fusefs.Engine{
		Resolver: resolver,
		Repo:     cfg,
		Overlay:  ov,
		Hydrator: h,
	}

	mfs, err := fusefs.MountRepoWithGate(cfg, resolver, engine, gate)
	if err != nil {
		s.logger.Error("fuse mount failed, running without FUSE", "repo", cfg.Name, "error", err)
		mfs = nil
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	state := cfg.PrepareState
	if strings.TrimSpace(state) == "" {
		state = model.PrepareStatePreparing
	}
	rt := &repoRuntime{
		cfg:      cfg,
		ctx:      runtimeCtx,
		cancel:   cancel,
		snapshot: snap,
		overlay:  ov,
		hydrator: h,
		resolver: resolver,
		mfs:      mfs,
		gate:     gate,
		state: model.RepoRuntimeState{
			RepoID:             cfg.ID,
			CurrentHEADOID:     headOID,
			CurrentHEADRef:     headRef,
			SnapshotGeneration: gen,
			LastFetchResult:    "never",
			State:              state,
			PrepareError:       cfg.PrepareError,
		},
	}
	s.startRuntime(rt)
	return nil
}

func (s *Service) startPrepareWorker(ctx context.Context, cfg model.RepoConfig) {
	s.mu.Lock()
	if s.preparing[cfg.ID] {
		s.mu.Unlock()
		return
	}
	s.preparing[cfg.ID] = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.preparing, cfg.ID)
			s.mu.Unlock()
		}()
		if err := s.runPrepare(ctx, cfg); err != nil {
			s.logger.Error("repo prepare failed", "repo", cfg.Name, "error", err)
		}
	}()
}

func (s *Service) runPrepare(ctx context.Context, cfg model.RepoConfig) error {
	s.fillPaths(&cfg)
	if strings.TrimSpace(cfg.FetchRef) == "" {
		cfg.FetchRef = cfg.Branch
	}

	fail := func(err error) error {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		_ = s.setPrepareState(ctx, cfg, model.PrepareStateFailed, err)
		return err
	}

	if cfg.PreparedGitDir {
		if err := s.git.ValidatePreparedGitDir(ctx, cfg); err != nil {
			return fail(err)
		}
		if err := s.git.FetchRefNonInteractive(ctx, cfg, cfg.FetchRef); err != nil {
			return fail(err)
		}
		if err := s.git.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef); err != nil {
			return fail(err)
		}
	} else {
		if strings.TrimSpace(cfg.RemoteURL) == "" {
			return fail(errors.New("remote URL is required for async clone"))
		}
		if err := s.git.CloneBloblessNonInteractive(ctx, cfg); err != nil {
			return fail(err)
		}
	}

	headOID, headRef, err := s.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	snap, closeSnap, err := s.snapshotForPrepare(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	if closeSnap {
		defer snap.Close()
	}
	gen, _, err := s.publishSnapshot(ctx, cfg, snap, headOID, headRef)
	if err != nil {
		return fail(err)
	}
	if err := s.completePreparedRuntime(ctx, cfg, headOID, headRef, gen); err != nil {
		return fail(err)
	}
	if err := s.setPrepareState(ctx, cfg, model.PrepareStateReady, nil); err != nil {
		return err
	}
	return nil
}

func (s *Service) snapshotForPrepare(ctx context.Context, cfg model.RepoConfig) (*snapshot.Store, bool, error) {
	s.mu.Lock()
	rt := s.running[cfg.ID]
	s.mu.Unlock()
	if rt != nil && rt.snapshot != nil {
		return rt.snapshot, false, nil
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		return nil, false, err
	}
	return snap, true, nil
}

func (s *Service) completePreparedRuntime(ctx context.Context, cfg model.RepoConfig, headOID string, headRef string, gen int64) error {
	s.mu.Lock()
	rt := s.running[cfg.ID]
	s.mu.Unlock()
	if rt == nil {
		return nil
	}
	baseLookup := func(path string) (model.BaseNode, bool) {
		return rt.snapshot.GetNode(gen, path)
	}
	if err := rt.overlay.Reconcile(ctx, baseLookup); err != nil {
		return err
	}
	if err := s.git.ReadTreeHEAD(ctx, cfg); err != nil {
		return err
	}
	s.refreshCommitTime(ctx, cfg, headOID, rt.resolver, "commit timestamp unavailable")
	rt.resolver.SetGeneration(gen)
	rt.gate.MarkReady()
	s.mu.Lock()
	rt.cfg = cfg
	rt.cfg.PrepareState = model.PrepareStateReady
	rt.cfg.PrepareError = ""
	setHeadState(&rt.state, headOID, headRef, gen)
	rt.state.State = repoStateMounted
	rt.state.PrepareError = ""
	s.mu.Unlock()
	s.startRepoBackground(rt)
	return nil
}

func (s *Service) setPrepareState(ctx context.Context, cfg model.RepoConfig, state string, stateErr error) error {
	msg := ""
	if stateErr != nil {
		msg = auth.RedactString(stateErr.Error())
	}
	if err := s.registry.UpdatePrepareState(ctx, cfg.ID, state, msg); err != nil {
		return err
	}
	s.mu.Lock()
	if rt, ok := s.running[cfg.ID]; ok {
		rt.cfg.PrepareState = state
		rt.cfg.PrepareError = msg
		rt.state.State = runtimeStateForPrepareState(state)
		rt.state.PrepareError = msg
		if rt.gate != nil {
			switch state {
			case model.PrepareStateFailed:
				rt.gate.MarkFailed(prepareGateError(msg))
			case model.PrepareStateReady:
				rt.gate.MarkReady()
			default:
				rt.gate.Reset()
			}
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) onHEADChanged(ctx context.Context, rt *repoRuntime) {
	oid, ref, err := s.git.ResolveHEAD(ctx, rt.cfg)
	if err != nil {
		s.logger.Error("HEAD resolve failed", "repo", rt.cfg.Name, "error", err)
		return
	}
	s.mu.Lock()
	prevOID := rt.state.CurrentHEADOID
	prevRef := rt.state.CurrentHEADRef
	s.mu.Unlock()
	if oid == prevOID {
		if ref == prevRef {
			return
		}
		if err := rt.snapshot.UpdateHEADRef(ctx, ref); err != nil {
			s.logger.Warn("snapshot head_ref update failed", "repo", rt.cfg.Name, "error", err)
		}
		s.mu.Lock()
		rt.state.CurrentHEADRef = ref
		s.mu.Unlock()
		return
	}
	gen, phase, err := s.publishSnapshot(ctx, rt.cfg, rt.snapshot, oid, ref)
	if err != nil {
		msg := "tree rebuild failed"
		if phase == "publish" {
			msg = "snapshot publish failed"
		}
		s.logger.Error(msg, "repo", rt.cfg.Name, "error", err)
		return
	}
	baseLookup := func(path string) (model.BaseNode, bool) {
		return rt.snapshot.GetNode(gen, path)
	}
	if err := rt.overlay.Reconcile(ctx, baseLookup); err != nil {
		s.logger.Warn("overlay reconcile failed", "repo", rt.cfg.Name, "error", err)
	}

	// Refresh the git index so `git status` inside the mount reflects the
	// new HEAD. Without this, the index still describes the old tree and
	// status shows phantom diffs after a branch switch or commit.
	if err := s.git.ReadTreeHEAD(ctx, rt.cfg); err != nil {
		s.logger.Warn("read-tree HEAD failed", "repo", rt.cfg.Name, "error", err)
	}
	s.refreshCommitTime(ctx, rt.cfg, oid, rt.resolver, "commit timestamp unavailable")

	// Atomically update the resolver's generation so FUSE ops see the new snapshot
	rt.resolver.SetGeneration(gen)
	s.mu.Lock()
	setHeadState(&rt.state, oid, ref, gen)
	s.mu.Unlock()
}

func (s *Service) refreshLoop(rt *repoRuntime) {
	backoff := rt.cfg.RefreshInterval
	const maxBackoff = 10 * time.Minute
	ticker := time.NewTicker(backoff)
	defer ticker.Stop()
	for {
		select {
		case <-rt.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(rt.ctx, 30*time.Second)
			err := s.git.Fetch(ctx, rt.cfg)
			if err != nil {
				s.mu.Lock()
				markFetchFailure(&rt.state, auth.RedactString(err.Error()))
				s.mu.Unlock()
				cancel()
				// Exponential backoff on failure, capped at maxBackoff
				backoff = min(backoff*2, maxBackoff)
				ticker.Reset(backoff)
				continue
			}
			state, abErr := s.fetchState(ctx, rt.cfg)
			cancel()
			// Reset backoff on success
			backoff = rt.cfg.RefreshInterval
			ticker.Reset(backoff)
			s.mu.Lock()
			markFetchResult(&rt.state, time.Now(), "ok")
			if abErr == nil {
				applyAheadBehind(&rt.state, state)
			}
			s.mu.Unlock()
		}
	}
}

func (s *Service) readPersistedStatus(ctx context.Context, cfg model.RepoConfig) model.RepoRuntimeState {
	// One-shot CLI process: reconstruct state from persisted stores and
	// OS-level mount check since we don't share memory with the daemon.
	st := model.RepoRuntimeState{RepoID: cfg.ID, State: repoStateUnmounted, LastFetchResult: "never", PrepareError: cfg.PrepareError}
	if isPendingOrFailedPrepareState(cfg.PrepareState) {
		st.State = cfg.PrepareState
	} else if isMounted(cfg.MountPath) {
		st.State = repoStateMounted
	}
	if cfg.MetaDBPath != "" {
		if snap, err := snapshot.New(ctx, cfg.MetaDBPath); err == nil {
			st.CurrentHEADOID, st.CurrentHEADRef, st.SnapshotGeneration, _ = snap.ReadState(ctx)
			snap.Close()
		}
	}
	if cfg.OverlayDBPath != "" {
		if _, statErr := os.Stat(cfg.OverlayDBPath); statErr == nil {
			if db, err := meta.OpenDB(cfg.OverlayDBPath); err == nil {
				var count int64
				if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM overlay_entries WHERE kind <> 'delete'`).Scan(&count); err == nil {
					st.DirtyOverlay = count > 0
				}
				db.Close()
			}
		}
	}
	// Best-effort last fetch time from FETCH_HEAD mtime.
	if fi, err := os.Stat(filepath.Join(cfg.GitDir, "FETCH_HEAD")); err == nil {
		st.LastFetchAt = fi.ModTime()
		st.LastFetchResult = "ok"
	}
	applyHydrationStats(&st, cfg.BlobCacheDir)
	return st
}

func (s *Service) publishSnapshot(ctx context.Context, cfg model.RepoConfig, snap *snapshot.Store, oid string, ref string) (int64, string, error) {
	nodes, err := s.git.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		return 0, "build", err
	}
	gen, err := snap.PublishGeneration(ctx, oid, ref, nodes)
	if err != nil {
		return 0, "publish", err
	}
	return gen, "", nil
}

func (s *Service) refreshCommitTime(ctx context.Context, cfg model.RepoConfig, oid string, resolver *fusefs.Resolver, warnMsg string) {
	if ts, err := s.git.CommitTimestamp(ctx, cfg, oid); err == nil {
		resolver.SetCommitTime(ts)
	} else {
		s.logger.Warn(warnMsg, "repo", cfg.Name, "error", err)
	}
}

func (s *Service) fetchState(ctx context.Context, cfg model.RepoConfig) (aheadBehind, error) {
	ahead, behind, diverged, err := s.git.ComputeAheadBehind(ctx, cfg)
	if err != nil {
		return aheadBehind{}, err
	}
	return aheadBehind{ahead: ahead, behind: behind, diverged: diverged}, nil
}

func (s *Service) startRuntime(rt *repoRuntime) {
	s.mu.Lock()
	s.running[rt.cfg.ID] = rt
	s.mu.Unlock()

	if rt.mfs != nil {
		go func() {
			_ = rt.mfs.Join(rt.ctx)
		}()
	}
}

func (s *Service) startRepoBackground(rt *repoRuntime) {
	s.mu.Lock()
	if rt.active {
		s.mu.Unlock()
		return
	}
	rt.active = true
	s.mu.Unlock()

	go s.refreshLoop(rt)

	w := watcher.New(500 * time.Millisecond)
	go w.Watch(rt.ctx, rt.cfg.GitDir, func() {
		s.onHEADChanged(rt.ctx, rt)
	})
}

func newRuntimeState(repoID model.RepoID, headOID string, headRef string, gen int64) model.RepoRuntimeState {
	return model.RepoRuntimeState{
		RepoID:             repoID,
		CurrentHEADOID:     headOID,
		CurrentHEADRef:     headRef,
		SnapshotGeneration: gen,
		LastFetchResult:    "never",
		State:              repoStateMounted,
	}
}

func setHeadState(st *model.RepoRuntimeState, oid string, ref string, gen int64) {
	st.CurrentHEADOID = oid
	st.CurrentHEADRef = ref
	st.SnapshotGeneration = gen
}

func applyAheadBehind(st *model.RepoRuntimeState, state aheadBehind) {
	st.AheadCount = state.ahead
	st.BehindCount = state.behind
	st.Diverged = state.diverged
}

func markFetchSuccess(st *model.RepoRuntimeState, at time.Time, state aheadBehind) {
	markFetchResult(st, at, "ok")
	applyAheadBehind(st, state)
}

func markFetchResult(st *model.RepoRuntimeState, at time.Time, result string) {
	st.LastFetchResult = result
	st.LastFetchAt = at
	if st.State == repoStateDegraded && result == "ok" {
		st.State = repoStateMounted
	}
}

func markFetchFailure(st *model.RepoRuntimeState, result string) {
	st.State = repoStateDegraded
	st.LastFetchResult = result
}

func applyHydrationStats(st *model.RepoRuntimeState, cacheDir string) {
	count, bytes := blobCacheStats(cacheDir)
	st.HydratedBlobCount = count
	st.HydratedBlobBytes = bytes
}

func blobCacheStats(cacheDir string) (int64, int64) {
	if strings.TrimSpace(cacheDir) == "" {
		return 0, 0
	}
	var count int64
	var bytes int64
	_ = filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		count++
		bytes += info.Size()
		return nil
	})
	return count, bytes
}

func (s *Service) unmount(id model.RepoID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.running[id]
	if !ok {
		return
	}
	s.stopRuntime(rt)
	delete(s.running, id)
}

func (s *Service) stopRuntime(rt *repoRuntime) {
	if rt.cancel != nil {
		rt.cancel()
	}
	if rt.mfs != nil {
		_ = rt.mfs.Unmount()
	}
	if rt.hydrator != nil {
		rt.hydrator.Stop()
	}
	_ = rt.snapshot.Close()
	_ = rt.overlay.Close()
}

func (s *Service) fillPaths(cfg *model.RepoConfig) {
	if cfg.MountRoot == "" {
		if s.mountRoot != "" {
			cfg.MountRoot = s.mountRoot
		} else {
			cfg.MountRoot = filepath.Join(s.root, "mnt")
		}
	}
	if cfg.MountPath == "" {
		cfg.MountPath = filepath.Join(cfg.MountRoot, cfg.Name)
	}
	if cfg.GitDir == "" {
		cfg.GitDir = filepath.Join(s.root, "repos", string(cfg.ID), "git")
	}
	if cfg.OverlayDir == "" {
		cfg.OverlayDir = filepath.Join(s.root, "overlays", string(cfg.ID))
	}
	if cfg.BlobCacheDir == "" {
		cfg.BlobCacheDir = filepath.Join(s.root, "cache", "blobs", string(cfg.ID))
	}
	if cfg.MetaDBPath == "" {
		cfg.MetaDBPath = filepath.Join(s.root, "meta", string(cfg.ID)+".sqlite")
	}
	if cfg.OverlayDBPath == "" {
		cfg.OverlayDBPath = filepath.Join(cfg.OverlayDir, "meta.sqlite")
	}
}

func ParseRefresh(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid refresh interval %q", v)
	}
	if d <= 0 {
		return 0, errors.New("refresh interval must be positive")
	}
	return d, nil
}

func isAsyncRepo(cfg model.RepoConfig) bool {
	return cfg.PreparedGitDir || strings.TrimSpace(cfg.PrepareState) != ""
}

func shouldMountAsync(cfg model.RepoConfig) bool {
	return isPendingOrFailedPrepareState(cfg.PrepareState)
}

func isPendingOrFailedPrepareState(state string) bool {
	switch strings.TrimSpace(state) {
	case model.PrepareStatePreparing, model.PrepareStateFailed:
		return true
	default:
		return false
	}
}

func runtimeStateForPrepareState(state string) string {
	if state == model.PrepareStateReady {
		return repoStateMounted
	}
	return state
}

func prepareGateError(msg string) error {
	if strings.TrimSpace(msg) == "" {
		return fusefs.ErrRepoNotReady
	}
	return errors.New(msg)
}
