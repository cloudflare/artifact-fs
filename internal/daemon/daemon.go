package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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
	"golang.org/x/sys/unix"
)

const DefaultHydrationConcurrency = 4

const (
	defaultPrepareTimeout    = 30 * time.Minute
	prepareStateWriteTimeout = 5 * time.Second
	repoConfigLockRetry      = 25 * time.Millisecond
	sizeUpdateFlushInterval  = 100 * time.Millisecond
)

const (
	repoStateMounted   = "mounted"
	repoStateUnmounted = "unmounted"
	repoStateDegraded  = "degraded"
)

const (
	prepareModeSync  = "sync"
	prepareModeAsync = "async"

	prepareSourceFreshClone     = "fresh_clone"
	prepareSourceExistingClone  = "existing_clone"
	prepareSourcePreparedGitDir = "prepared_git_dir"
	prepareSourceVerified       = "verified_source"

	preparePhaseUnknown           = "unknown"
	preparePhaseValidate          = "validate"
	preparePhaseInitializeStorage = "initialize_storage"
	preparePhaseClone             = "clone"
	preparePhaseConfigureRemote   = "configure_remote"
	preparePhaseFetch             = "fetch"
	preparePhaseUpdateBranch      = "update_branch"
	preparePhaseResolveHEAD       = "resolve_head"
	preparePhaseOpenSnapshot      = "open_snapshot"
	preparePhaseBuildTree         = "build_tree"
	preparePhasePublishSnapshot   = "publish_snapshot"
	preparePhasePersistReady      = "persist_ready"
	preparePhasePersistPreparing  = "persist_preparing"
	preparePhaseActivateRuntime   = "activate_runtime"
	preparePhaseCleanupSnapshot   = "cleanup_snapshot"
	preparePhaseComplete          = "complete"
	snapshotPhaseBuild            = "build"
	snapshotPhasePublish          = "publish"

	prepareLogStateStarted   = "started"
	prepareLogStateCompleted = "completed"
	prepareLogStateFailed    = "failed"
	prepareLogStateCanceled  = "canceled"

	logRepoPreparationStarted    = "repo preparation started"
	logRepoPreparationCompleted  = "repo preparation completed"
	logRepoPreparationFailed     = "repo preparation failed"
	logRepoPreparationCanceled   = "repo preparation canceled"
	logRepoPrepareStatusWriteErr = "repo prepare status write failed"
	logRepoConfigChanged         = "repo config changed, remounting"
)

type Service struct {
	root                 string
	mountRoot            string
	hydrationConcurrency int
	prepareTimeout       time.Duration
	logger               *slog.Logger
	registry             *registry.Store
	git                  *gitstore.Store
	mu                   sync.Mutex
	running              map[model.RepoID]*repoRuntime
	preparing            map[model.RepoID]int64
	prepareAttempts      map[model.RepoID]int64
	prepareSeq           int64
	mountFailures        map[model.RepoID]*mountFailure
	prepareWorkers       sync.WaitGroup
	closing              bool
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
	sizes    *sizeUpdateBatcher
	resolver *fusefs.Resolver
	engine   *fusefs.Engine
	mfs      fusefs.MountedFS
	gate     *fusefs.ReadyGate
	state    model.RepoRuntimeState
	active   bool
	refresh  chan time.Duration
	joinDone chan struct{}
	stopping bool
	detached bool
	headMu   sync.Mutex
	workers  sync.WaitGroup
	mounts   sync.WaitGroup
}

type aheadBehind struct {
	ahead    int
	behind   int
	diverged bool
}

type AddRepoOptions struct {
	Async bool
}

type preparationPhaseError struct {
	phase string
	err   error
}

func (e *preparationPhaseError) Error() string { return e.err.Error() }
func (e *preparationPhaseError) Unwrap() error { return e.err }

func withPreparationPhase(phase string, err error) error {
	if err == nil {
		return nil
	}
	return &preparationPhaseError{phase: phase, err: err}
}

func preparationFailurePhase(err error) string {
	var phaseErr *preparationPhaseError
	if errors.As(err, &phaseErr) {
		return phaseErr.phase
	}
	return preparePhaseUnknown
}

func (s *Service) withRepoConfigLock(ctx context.Context, repoName string, fn func() error) error {
	return s.withRepoLock(ctx, repoName+".lock", fn)
}

func (s *Service) withRepoPrepareLock(ctx context.Context, repoName string, fn func() error) error {
	return s.withRepoLock(ctx, repoName+".prepare.lock", fn)
}

func (s *Service) withRepoLock(ctx context.Context, lockName string, fn func() error) error {
	lockDir := filepath.Join(s.root, "config", "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(filepath.Join(lockDir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	for {
		err = unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		timer := time.NewTimer(repoConfigLockRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	return fn()
}

func (s *Service) configVersionIsCurrent(ctx context.Context, cfg model.RepoConfig) (bool, error) {
	latest, err := s.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		return false, err
	}
	return latest.ConfigVersion == cfg.ConfigVersion, nil
}

func New(ctx context.Context, root string, logger *slog.Logger) (*Service, error) {
	reg, err := registry.New(ctx, filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		return nil, err
	}
	svc := &Service{
		root:            root,
		logger:          logger,
		registry:        reg,
		git:             gitstore.New(logger),
		prepareTimeout:  defaultPrepareTimeout,
		running:         map[model.RepoID]*repoRuntime{},
		preparing:       map[model.RepoID]int64{},
		prepareAttempts: map[model.RepoID]int64{},
		mountFailures:   map[model.RepoID]*mountFailure{},
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
	s.closing = true
	ids := make([]model.RepoID, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := s.unmount(id); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	s.prepareWorkers.Wait()
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
			if err := s.unmount(repo.ID); err != nil {
				s.logger.Error("repo unmount failed", "repo", repo.Name, "error", err)
			}
			continue
		}
		s.mu.Lock()
		rt, running := s.running[repo.ID]
		_, alreadyPreparing := s.preparing[repo.ID]
		runningConfigVersion := ""
		if running {
			runningConfigVersion = rt.cfg.ConfigVersion
		}
		s.mu.Unlock()
		if running && runningConfigVersion != repo.ConfigVersion {
			s.logger.InfoContext(ctx, logRepoConfigChanged, "repo", auth.RedactLogString(repo.Name))
			if err := s.unmount(repo.ID); err != nil {
				s.logger.Error("repo prepare remount unmount failed", "repo", repo.Name, "error", err)
				continue
			}
			s.supersedePrepare(repo.ID)
			delete(s.mountFailures, repo.ID)
			running = false
			alreadyPreparing = false
		}
		if running {
			s.retryRuntimeMount(rt)
			s.updateRuntimeRefresh(rt, repo.RefreshInterval, repo.RemoteRefreshDisabled)
			s.restartRunningPrepareIfCurrent(ctx, repo, rt, alreadyPreparing)
			continue
		}
		if repo.PrepareState == model.PrepareStateSyncPreparing {
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
		if err := s.unmount(id); err != nil {
			s.logger.Error("removed repo unmount failed", "repo", id, "error", err)
			continue
		}
		delete(s.mountFailures, id)
	}
	for id := range s.mountFailures {
		if !registered[id] {
			delete(s.mountFailures, id)
		}
	}

	return nil
}

func (s *Service) restartRunningPrepareIfCurrent(ctx context.Context, repo model.RepoConfig, rt *repoRuntime, alreadyPreparing bool) {
	if repo.PrepareState != model.PrepareStatePreparing || rt == nil {
		return
	}
	latest, err := s.registry.GetRepo(ctx, repo.Name)
	if err != nil {
		s.logger.Error("repo prepare state refresh failed", "repo", repo.Name, "error", err)
		return
	}
	if latest.PrepareState != model.PrepareStatePreparing {
		return
	}
	s.mu.Lock()
	if s.running[repo.ID] != rt {
		s.mu.Unlock()
		return
	}
	runtimeCfg := rt.cfg
	active := rt.active
	s.mu.Unlock()
	configMatches := samePrepareConfig(runtimeCfg, latest)
	if alreadyPreparing && configMatches {
		return
	}
	if active || !configMatches {
		if err := s.unmount(latest.ID); err != nil {
			s.logger.Error("repo prepare remount unmount failed", "repo", latest.Name, "error", err)
			return
		}
		if err := s.mountAsyncRepo(ctx, latest); err != nil {
			s.logger.Error("repo async remount failed", "repo", latest.Name, "error", err)
			return
		}
		s.supersedePrepare(latest.ID)
		s.startPrepareWorker(ctx, latest)
		return
	}
	if alreadyPreparing {
		return
	}
	if s.resetRunningPrepareState(latest) {
		s.startPrepareWorker(ctx, latest)
	}
}

func samePrepareConfig(a model.RepoConfig, b model.RepoConfig) bool {
	return a.Branch == b.Branch &&
		a.RemoteURL == b.RemoteURL &&
		a.RequiredCommit == b.RequiredCommit &&
		a.HistoryDepth == b.HistoryDepth &&
		a.RemoteRefreshDisabled == b.RemoteRefreshDisabled &&
		a.PreparedGitDir == b.PreparedGitDir &&
		a.FetchRef == b.FetchRef &&
		a.GitDir == b.GitDir &&
		a.MetaDBPath == b.MetaDBPath &&
		a.OverlayDir == b.OverlayDir &&
		a.OverlayDBPath == b.OverlayDBPath &&
		a.BlobCacheDir == b.BlobCacheDir &&
		a.MountPath == b.MountPath &&
		a.ConfigVersion == b.ConfigVersion
}

func normalizeSourceConfig(cfg *model.RepoConfig) error {
	cfg.Branch = strings.TrimSpace(cfg.Branch)
	if cfg.Branch == "" {
		cfg.Branch = "refs/heads/main"
	} else if !strings.HasPrefix(cfg.Branch, "refs/") {
		cfg.Branch = "refs/heads/" + cfg.Branch
	}
	cfg.RequiredCommit = strings.ToLower(strings.TrimSpace(cfg.RequiredCommit))
	if cfg.HistoryDepth < 0 {
		return errors.New("--depth must not be negative")
	}
	if cfg.RequiredCommit == "" {
		return nil
	}
	if len(cfg.RequiredCommit) != 40 && len(cfg.RequiredCommit) != 64 {
		return errors.New("--require-commit must be a full 40- or 64-character commit OID")
	}
	for _, character := range cfg.RequiredCommit {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("--require-commit must be hexadecimal")
		}
	}
	return nil
}

func (s *Service) prepareConfigStillCurrent(ctx context.Context, cfg model.RepoConfig) bool {
	latest, err := s.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		s.logger.Error("repo prepare state refresh failed", "repo", cfg.Name, "error", err)
		return false
	}
	s.fillPaths(&latest)
	if strings.TrimSpace(latest.FetchRef) == "" {
		latest.FetchRef = defaultFetchRef(latest.Branch)
	}
	return samePrepareConfig(cfg, latest)
}

func (s *Service) AddRepo(ctx context.Context, cfg model.RepoConfig) error {
	return s.AddRepoWithOptions(ctx, cfg, AddRepoOptions{})
}

func (s *Service) AddRepoWithOptions(ctx context.Context, cfg model.RepoConfig, opts AddRepoOptions) error {
	if err := model.ValidateRepoName(cfg.Name); err != nil {
		return err
	}
	if err := normalizeSourceConfig(&cfg); err != nil {
		return err
	}
	if cfg.ID == "" {
		cfg.ID = model.RepoID(cfg.Name)
	}
	cfg.RemoteURLRedacted = auth.RedactRemoteURL(cfg.RemoteURL)
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 30 * time.Second
	}
	explicitGitDir := strings.TrimSpace(cfg.GitDir) != ""
	s.fillPaths(&cfg)
	if strings.TrimSpace(cfg.FetchRef) == "" {
		cfg.FetchRef = defaultFetchRef(cfg.Branch)
	}
	cfg.ConfigVersion = rand.Text()
	cloneURL := cfg.RemoteURL
	if cfg.PreparedGitDir && !opts.Async {
		return fmt.Errorf("--prepared-gitdir requires --async")
	}
	if cfg.PreparedGitDir && cfg.RequiredCommit != "" {
		return fmt.Errorf("--prepared-gitdir is not supported with --require-commit")
	}
	if opts.Async {
		if strings.TrimSpace(cfg.RemoteURL) == "" && !cfg.PreparedGitDir {
			return fmt.Errorf("--remote is required unless --prepared-gitdir is set")
		}
		if cfg.PreparedGitDir && !explicitGitDir {
			return fmt.Errorf("--git-dir is required with --prepared-gitdir")
		}
		if auth.HasInlineCredentials(cfg.RemoteURL) {
			return fmt.Errorf("async repositories must use ambient credentials; remove credentials from --remote")
		}
		cfg.PrepareState = model.PrepareStatePreparing
		cfg.PrepareError = ""
	} else {
		cfg.PrepareState = model.PrepareStateSyncPreparing
		cfg.PrepareError = ""
		if auth.HasInlineCredentials(cfg.RemoteURL) {
			cfg.RemoteURL = ""
		}
	}
	registeredCfg := cfg
	if err := s.withRepoConfigLock(ctx, cfg.Name, func() error {
		return s.registry.AddRepo(ctx, cfg)
	}); err != nil {
		return err
	}
	if opts.Async {
		return nil
	}
	// Clone and build snapshot so the repo is ready to mount, but don't start
	// the FUSE server -- that's the daemon's job.
	cfg.RemoteURL = cloneURL
	prepareCtx, cancel := context.WithTimeout(ctx, s.prepareTimeoutDuration())
	err := s.prepareRepo(prepareCtx, cfg)
	prepareContextErr := prepareCtx.Err()
	cancel()
	if err == nil {
		return s.persistSyncPrepareSuccess(ctx, registeredCfg)
	}
	if prepareContextErr != nil && !errors.Is(err, prepareContextErr) {
		err = errors.Join(err, prepareContextErr)
	}
	return s.persistSyncPrepareFailure(ctx, cfg, registeredCfg, err)
}

func (s *Service) persistSyncPrepareFailure(ctx context.Context, cfg model.RepoConfig, registeredCfg model.RepoConfig, prepareErr error) error {
	stateErr := prepareErr
	if errors.Is(prepareErr, context.DeadlineExceeded) {
		stateErr = errors.New("prepare timed out")
	} else if errors.Is(prepareErr, context.Canceled) {
		stateErr = errors.New("prepare canceled")
	}
	stateCtx, stateCancel := context.WithTimeout(context.WithoutCancel(ctx), prepareStateWriteTimeout)
	defer stateCancel()
	if stateWriteErr := s.registry.UpdatePrepareStateForConfig(stateCtx, registeredCfg, "", auth.RedactLogString(stateErr.Error(), cfg.RemoteURL)); stateWriteErr != nil {
		if errors.Is(stateWriteErr, registry.ErrRepoChanged) {
			return prepareErr
		}
		s.logger.ErrorContext(stateCtx, logRepoPrepareStatusWriteErr, "repo", auth.RedactLogString(cfg.Name), "mode", prepareModeSync, "phase", preparationFailurePhase(prepareErr), "target_state", "", "error", auth.RedactLogString(stateWriteErr.Error()))
		return errors.Join(prepareErr, fmt.Errorf("persist failed prepare state: %w", stateWriteErr))
	}
	return prepareErr
}

func (s *Service) persistSyncPrepareSuccess(ctx context.Context, cfg model.RepoConfig) error {
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareStateWriteTimeout)
	defer cancel()
	if err := s.registry.UpdatePrepareStateForConfig(stateCtx, cfg, "", ""); err != nil {
		s.logger.ErrorContext(stateCtx, logRepoPrepareStatusWriteErr, "repo", auth.RedactLogString(cfg.Name), "mode", prepareModeSync, "phase", preparePhaseComplete, "target_state", "", "error", auth.RedactLogString(err.Error()))
		return err
	}
	return nil
}

func (s *Service) RemoveRepo(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if err := s.unmount(cfg.ID); err != nil {
		return err
	}
	return s.withRepoConfigLock(ctx, cfg.Name, func() error {
		latest, err := s.registry.GetRepo(ctx, name)
		if err != nil {
			return err
		}
		if latest.ConfigVersion != cfg.ConfigVersion {
			return registry.ErrRepoChanged
		}
		return s.registry.RemoveRepo(ctx, name)
	})
}

func (s *Service) ListRepos(ctx context.Context) ([]model.RepoConfig, error) {
	return s.registry.ListRepos(ctx)
}

func (s *Service) SetRefresh(ctx context.Context, name string, interval time.Duration, disabled bool) error {
	if interval <= 0 && !disabled {
		return errors.New("refresh interval must be positive")
	}
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	cfg.RefreshInterval = interval
	cfg.RemoteRefreshDisabled = disabled
	if err := s.registry.UpdateRefresh(ctx, cfg.ID, interval, disabled); err != nil {
		return err
	}
	s.mu.Lock()
	rt := s.running[cfg.ID]
	s.mu.Unlock()
	s.updateRuntimeRefresh(rt, interval, disabled)
	return nil
}

func (s *Service) updateRuntimeRefresh(rt *repoRuntime, interval time.Duration, disabled bool) {
	if rt == nil {
		return
	}
	s.mu.Lock()
	if rt.cfg.RefreshInterval == interval && rt.cfg.RemoteRefreshDisabled == disabled {
		s.mu.Unlock()
		return
	}
	rt.cfg.RefreshInterval = interval
	rt.cfg.RemoteRefreshDisabled = disabled
	refresh := rt.refresh
	s.mu.Unlock()
	if refresh != nil && interval > 0 {
		select {
		case refresh <- interval:
		default:
			select {
			case <-refresh:
			default:
			}
			select {
			case refresh <- interval:
			default:
			}
		}
	}
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
		blobCacheDir := rt.cfg.BlobCacheDir
		s.mu.Unlock()
		applyHydrationStats(&st, blobCacheDir)
		applySourceStatus(&st, cfg)
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
	if cfg.PrepareState == model.PrepareStatePreparing || cfg.PrepareState == model.PrepareStateSyncPreparing {
		return fusefs.ErrRepoNotReady
	}
	if cfg.PrepareState == model.PrepareStateFailed {
		return fmt.Errorf("repo prepare failed: %s", cfg.PrepareError)
	}
	if cfg.RemoteRefreshDisabled {
		return errors.New("remote refresh is disabled for this repository")
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
		markFetchSuccess(&rt.state, time.Now(), state, rt.mfs != nil)
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) Prepare(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	async := isAsyncRepo(cfg)
	if async && cfg.PrepareState == model.PrepareStateReady && cfg.RequiredCommit == "" {
		return nil
	}
	nextVersion := rand.Text()
	nextState := model.PrepareStateSyncPreparing
	if async {
		nextState = model.PrepareStatePreparing
	}
	if err := s.registry.BeginPrepare(ctx, cfg, nextState, nextVersion); err != nil {
		return err
	}
	cfg.ConfigVersion = nextVersion
	cfg.PrepareState = nextState
	cfg.PrepareError = ""
	if !async {
		prepareCtx, cancel := context.WithTimeout(ctx, s.prepareTimeoutDuration())
		err := s.prepareRepo(prepareCtx, cfg)
		prepareContextErr := prepareCtx.Err()
		cancel()
		if err != nil {
			if prepareContextErr != nil && !errors.Is(err, prepareContextErr) {
				err = errors.Join(err, prepareContextErr)
			}
			return s.persistSyncPrepareFailure(ctx, cfg, cfg, err)
		}
		return s.persistSyncPrepareSuccess(ctx, cfg)
	}
	if strings.TrimSpace(cfg.FetchRef) == "" {
		cfg.FetchRef = defaultFetchRef(cfg.Branch)
	}
	if s.resetRunningPrepareState(cfg) {
		s.startPrepareWorker(ctx, cfg)
	}
	return nil
}

func (s *Service) Remount(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if err := s.unmount(cfg.ID); err != nil {
		return err
	}
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
	return s.unmount(cfg.ID)
}

// prepareRepo clones the git repo and builds the initial snapshot. It does NOT
// start a FUSE mount or any background goroutines, so it's safe to call from
// short-lived CLI commands like add-repo.
func (s *Service) prepareRepo(ctx context.Context, cfg model.RepoConfig) (retErr error) {
	started := time.Now()
	safeRepo := auth.RedactLogString(cfg.Name)
	safeBranch := auth.RedactLogString(cfg.Branch)
	safeFetchRef := auth.RedactLogString(cfg.FetchRef)
	source := prepareSourceFreshClone
	existingClone := false
	if cfg.RequiredCommit != "" {
		source = prepareSourceVerified
	} else if _, err := os.Stat(cfg.GitDir); err == nil {
		source = prepareSourceExistingClone
		existingClone = true
	}
	deadlineSet := false
	timeoutMS := int64(0)
	if deadline, ok := ctx.Deadline(); ok {
		deadlineSet = true
		timeoutMS = max(time.Until(deadline).Milliseconds(), 0)
	}
	logger := s.logger.With("repo", safeRepo, "mode", prepareModeSync, "attempt", 1)
	logger.InfoContext(ctx, logRepoPreparationStarted, "source", source, "phase", preparePhaseValidate, "state", prepareLogStateStarted, "duration_ms", 0, "branch", safeBranch, "fetch_ref", safeFetchRef, "deadline_set", deadlineSet, "timeout_ms", timeoutMS)

	var headOID, headRef string
	var gen int64
	defer func() {
		durationMS := time.Since(started).Milliseconds()
		if retErr == nil {
			logger.InfoContext(ctx, logRepoPreparationCompleted, "source", source, "phase", preparePhaseComplete, "state", prepareLogStateCompleted, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "head_oid", auth.RedactLogString(headOID), "head_ref", auth.RedactLogString(headRef), "snapshot_generation", gen)
			return
		}
		phase := preparationFailurePhase(retErr)
		timedOut := errors.Is(retErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)
		canceled := errors.Is(retErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
		state := prepareLogStateFailed
		if canceled && !timedOut {
			state = prepareLogStateCanceled
		}
		args := []any{"source", source, "phase", phase, "state", state, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "timed_out", timedOut, "canceled", canceled, "error", auth.RedactLogString(retErr.Error(), cfg.RemoteURL)}
		if canceled && !timedOut {
			logger.InfoContext(ctx, logRepoPreparationCanceled, args...)
			return
		}
		logger.ErrorContext(ctx, logRepoPreparationFailed, args...)
	}()
	return s.withRepoPrepareLock(ctx, cfg.Name, func() error {
		current, err := s.configVersionIsCurrent(ctx, cfg)
		if err != nil {
			return withPreparationPhase(preparePhaseValidate, err)
		}
		if !current {
			return withPreparationPhase(preparePhaseValidate, fmt.Errorf("prepare superseded by newer repo config: %w", registry.ErrRepoChanged))
		}
		existingClone = false
		if _, err := os.Stat(cfg.GitDir); err == nil {
			existingClone = true
			source = prepareSourceExistingClone
		} else if !errors.Is(err, os.ErrNotExist) {
			return withPreparationPhase(preparePhaseValidate, err)
		} else if cfg.RequiredCommit == "" {
			source = prepareSourceFreshClone
		}
		if existingClone && strings.TrimSpace(cfg.RemoteURL) != "" {
			if err := s.git.ConfigureRemoteWithCredentials(ctx, cfg); err != nil {
				return withPreparationPhase(preparePhaseConfigureRemote, err)
			}
			if err := s.git.FetchRefWithCredentials(ctx, cfg, cfg.FetchRef); err != nil {
				return withPreparationPhase(preparePhaseFetch, err)
			}
			if err := s.git.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef); err != nil {
				return withPreparationPhase(preparePhaseUpdateBranch, err)
			}
		}

		snap, resolvedOID, resolvedRef, generation, err := s.ensurePreparedRepo(ctx, cfg, false)
		if err != nil {
			return err
		}
		headOID, headRef, gen = resolvedOID, resolvedRef, generation
		defer snap.Close()
		return nil
	})
}

func (s *Service) prepareSource(ctx context.Context, cfg model.RepoConfig) (model.PreparedSource, error) {
	if cfg.RequiredCommit != "" {
		return s.git.PrepareSource(ctx, cfg, model.SourceRequirement{
			Ref:            cfg.Branch,
			RequiredCommit: cfg.RequiredCommit,
			Depth:          cfg.HistoryDepth,
		})
	}
	if err := s.git.CloneBlobless(ctx, cfg); err != nil {
		return model.PreparedSource{}, err
	}
	headOID, headRef, err := s.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		return model.PreparedSource{}, err
	}
	return model.PreparedSource{Ref: headRef, Commit: headOID}, nil
}

func (s *Service) recordAcquisition(ctx context.Context, cfg model.RepoConfig, source model.PreparedSource) error {
	if !source.Verified || (!source.Acquired && cfg.AcquiredRef == source.Ref && strings.EqualFold(cfg.AcquiredCommit, source.Commit)) {
		return nil
	}
	return s.registry.RecordAcquisition(ctx, cfg, source)
}

// ensurePreparedRepo makes sure the repo is cloned and has an initial snapshot.
// The returned snapshot store remains open for callers that need to continue
// into runtime startup.
func (s *Service) ensurePreparedRepo(ctx context.Context, cfg model.RepoConfig, pruneOldGenerations bool) (*snapshot.Store, string, string, int64, error) {
	if err := os.MkdirAll(cfg.MountPath, 0o755); err != nil {
		return nil, "", "", 0, withPreparationPhase(preparePhaseInitializeStorage, err)
	}
	source, err := s.prepareSource(ctx, cfg)
	if err != nil {
		return nil, "", "", 0, withPreparationPhase(preparePhaseClone, err)
	}
	headOID, headRef := source.Commit, source.Ref
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		return nil, "", "", 0, withPreparationPhase(preparePhaseOpenSnapshot, err)
	}
	gen := int64(0)
	failurePhase := preparePhaseValidate
	err = s.withRepoConfigLock(ctx, cfg.Name, func() error {
		current, err := s.configVersionIsCurrent(ctx, cfg)
		if err != nil {
			return err
		}
		if !current {
			return registry.ErrRepoChanged
		}
		storedOID, storedRef, storedGen, readErr := snap.ReadState(ctx)
		gen = storedGen
		if readErr != nil || gen == 0 || storedOID != headOID || storedRef != headRef {
			failurePhase = preparePhaseBuildTree
			var publishPhase string
			gen, publishPhase, err = s.publishSnapshot(ctx, cfg, snap, headOID, headRef)
			if publishPhase == snapshotPhasePublish {
				failurePhase = preparePhasePublishSnapshot
			}
			if err != nil {
				return err
			}
		}
		if pruneOldGenerations {
			failurePhase = preparePhaseCleanupSnapshot
			return snap.PruneGenerations(ctx, gen)
		}
		return nil
	})
	if err != nil {
		snap.Close()
		return nil, "", "", 0, withPreparationPhase(failurePhase, err)
	}
	if err := s.recordAcquisition(ctx, cfg, source); err != nil {
		snap.Close()
		return nil, "", "", 0, withPreparationPhase(preparePhasePersistReady, err)
	}
	return snap, headOID, headRef, gen, nil
}

// mountRepo opens all stores, starts the FUSE server, watcher, and refresh
// loop. Called by the daemon's Start for each registered repo.
func (s *Service) mountRepo(ctx context.Context, cfg model.RepoConfig) error {
	var snap *snapshot.Store
	var headOID, headRef string
	var gen int64
	prepareCtx, cancel := context.WithTimeout(ctx, s.prepareTimeoutDuration())
	defer cancel()
	err := s.withRepoPrepareLock(prepareCtx, cfg.Name, func() error {
		latest, err := s.registry.GetRepo(prepareCtx, cfg.Name)
		if err != nil {
			return err
		}
		if latest.ConfigVersion != cfg.ConfigVersion {
			return registry.ErrRepoChanged
		}
		cfg = latest
		if cfg.PrepareState == model.PrepareStateSyncPreparing {
			return fusefs.ErrRepoNotReady
		}
		if cfg.PrepareState == "" && cfg.PrepareError != "" {
			return fmt.Errorf("repo prepare failed: %s", cfg.PrepareError)
		}
		snap, headOID, headRef, gen, err = s.ensurePreparedRepo(prepareCtx, cfg, true)
		if err != nil {
			return err
		}
		if cfg.RequiredCommit == "" {
			if err := s.git.EnsureIndexInitialized(prepareCtx, cfg); err != nil {
				snap.Close()
				snap = nil
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		snap.Close()
		return err
	}
	baseLookup := func(path string) (model.BaseNode, bool, error) {
		return snap.LookupNode(ctx, gen, path)
	}
	if err := ov.ReconcileChecked(ctx, baseLookup); err != nil {
		ov.Close()
		snap.Close()
		return err
	}
	h := hydrator.New(s.git)

	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	s.refreshCommitTime(ctx, cfg, headOID, resolver, "commit timestamp unavailable, mtime will use generation fallback")
	runtimeCtx, cancel := context.WithCancel(ctx)
	sizes := newSizeUpdateBatcher(snap, s.logger, cfg.Name)
	sizes.Start(runtimeCtx)

	h.SetOnHydrated(func(_ model.RepoID, objectOID string, size int64) {
		sizes.Add(resolver.Generation(), objectOID, size)
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
		s.logger.Error("fuse mount failed, runtime will retry", "repo", cfg.Name, "error", err)
		mfs = nil
	} else {
		s.configureStatusOptimization(ctx, cfg)
	}
	rt := &repoRuntime{
		cfg:      cfg,
		ctx:      runtimeCtx,
		cancel:   cancel,
		snapshot: snap,
		overlay:  ov,
		hydrator: h,
		sizes:    sizes,
		resolver: resolver,
		engine:   engine,
		mfs:      mfs,
		refresh:  make(chan time.Duration, 1),
		state:    newRuntimeState(cfg.ID, headOID, headRef, gen),
	}
	if mfs == nil {
		rt.state.State = repoStateDegraded
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
	runtimeCtx, cancel := context.WithCancel(ctx)
	sizes := newSizeUpdateBatcher(snap, s.logger, cfg.Name)
	sizes.Start(runtimeCtx)
	h.SetOnHydrated(func(_ model.RepoID, objectOID string, size int64) {
		sizes.Add(resolver.Generation(), objectOID, size)
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
		s.logger.Error("fuse mount failed, runtime will retry", "repo", cfg.Name, "error", err)
		mfs = nil
	}
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
		sizes:    sizes,
		resolver: resolver,
		engine:   engine,
		mfs:      mfs,
		gate:     gate,
		refresh:  make(chan time.Duration, 1),
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
	if s.closing {
		s.mu.Unlock()
		return
	}
	if _, ok := s.preparing[cfg.ID]; ok {
		s.mu.Unlock()
		return
	}
	s.prepareSeq++
	token := s.prepareSeq
	s.prepareAttempts[cfg.ID]++
	attempt := s.prepareAttempts[cfg.ID]
	workerCtx := ctx
	rt := s.running[cfg.ID]
	trackedRuntime := rt != nil && rt.ctx != nil && !rt.stopping
	if trackedRuntime {
		workerCtx = rt.ctx
		rt.workers.Add(1)
	}
	s.preparing[cfg.ID] = token
	s.prepareWorkers.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.prepareWorkers.Done()
		if trackedRuntime {
			defer rt.workers.Done()
		}
		defer func() {
			s.mu.Lock()
			if s.preparing[cfg.ID] == token {
				delete(s.preparing, cfg.ID)
			}
			s.mu.Unlock()
		}()
		prepareCtx, cancel := context.WithTimeout(workerCtx, s.prepareTimeoutDuration())
		defer cancel()
		_ = s.runPrepareAttempt(prepareCtx, cfg, attempt)
	}()
}

func (s *Service) supersedePrepare(id model.RepoID) {
	s.mu.Lock()
	delete(s.preparing, id)
	s.mu.Unlock()
}

func (s *Service) prepareTimeoutDuration() time.Duration {
	if s.prepareTimeout > 0 {
		return s.prepareTimeout
	}
	return defaultPrepareTimeout
}

func (s *Service) resetRunningPrepareState(cfg model.RepoConfig) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.running[cfg.ID]
	if !ok || rt.gate == nil {
		return false
	}
	rt.gate.Reset()
	rt.cfg = cfg
	rt.state.State = model.PrepareStatePreparing
	rt.state.PrepareError = ""
	return true
}

func (s *Service) runPrepare(ctx context.Context, cfg model.RepoConfig) error {
	return s.runPrepareAttempt(ctx, cfg, 1)
}

func (s *Service) runPrepareAttempt(ctx context.Context, cfg model.RepoConfig, attempt int64) (retErr error) {
	s.fillPaths(&cfg)
	if strings.TrimSpace(cfg.FetchRef) == "" {
		cfg.FetchRef = defaultFetchRef(cfg.Branch)
	}
	started := time.Now()
	safeRepo := auth.RedactLogString(cfg.Name)
	safeBranch := auth.RedactLogString(cfg.Branch)
	safeFetchRef := auth.RedactLogString(cfg.FetchRef)
	phase := preparePhaseValidate
	source := prepareSourceFreshClone
	if cfg.RequiredCommit != "" {
		source = prepareSourceVerified
	} else if cfg.PreparedGitDir {
		source = prepareSourcePreparedGitDir
	} else if _, err := os.Stat(cfg.GitDir); err == nil {
		source = prepareSourceExistingClone
	}
	deadlineSet := false
	timeoutMS := int64(0)
	if deadline, ok := ctx.Deadline(); ok {
		deadlineSet = true
		timeoutMS = max(time.Until(deadline).Milliseconds(), 0)
	}
	logger := s.logger.With("repo", safeRepo, "mode", prepareModeAsync, "attempt", attempt)
	logger.InfoContext(ctx, logRepoPreparationStarted, "source", source, "phase", preparePhaseValidate, "state", prepareLogStateStarted, "duration_ms", 0, "branch", safeBranch, "fetch_ref", safeFetchRef, "deadline_set", deadlineSet, "timeout_ms", timeoutMS)

	var headOID, headRef string
	var gen int64
	defer func() {
		durationMS := time.Since(started).Milliseconds()
		if retErr == nil {
			logger.InfoContext(ctx, logRepoPreparationCompleted, "source", source, "phase", preparePhaseComplete, "state", prepareLogStateCompleted, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "head_oid", auth.RedactLogString(headOID), "head_ref", auth.RedactLogString(headRef), "snapshot_generation", gen)
			return
		}
		timedOut := errors.Is(retErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)
		canceled := errors.Is(retErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
		state := prepareLogStateFailed
		if canceled && !timedOut {
			state = prepareLogStateCanceled
		}
		args := []any{"source", source, "phase", phase, "state", state, "duration_ms", durationMS, "deadline_set", deadlineSet, "timeout_ms", timeoutMS, "timed_out", timedOut, "canceled", canceled, "error", auth.RedactLogString(retErr.Error(), cfg.RemoteURL)}
		if canceled && !timedOut {
			logger.InfoContext(ctx, logRepoPreparationCanceled, args...)
			return
		}
		logger.ErrorContext(ctx, logRepoPreparationFailed, args...)
	}()

	fail := func(err error) error {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return err
		}
		stateErr := err
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			stateErr = errors.New("prepare timed out")
		}
		stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareStateWriteTimeout)
		defer cancel()
		if stateWriteErr := s.setPrepareState(stateCtx, cfg, model.PrepareStateFailed, stateErr); stateWriteErr != nil {
			if errors.Is(stateWriteErr, registry.ErrRepoChanged) {
				return err
			}
			logger.ErrorContext(stateCtx, logRepoPrepareStatusWriteErr, "phase", phase, "target_state", model.PrepareStateFailed, "error", auth.RedactLogString(stateWriteErr.Error()))
			return errors.Join(err, fmt.Errorf("persist failed prepare state: %w", stateWriteErr))
		}
		return err
	}
	lockAcquired := false
	retErr = s.withRepoPrepareLock(ctx, cfg.Name, func() error {
		lockAcquired = true
		current, err := s.configVersionIsCurrent(ctx, cfg)
		if err != nil {
			return fail(err)
		}
		if !current {
			return fmt.Errorf("prepare superseded by newer repo config: %w", registry.ErrRepoChanged)
		}
		var preparedSource model.PreparedSource
		prepareExistingClone := func() error {
			phase = preparePhaseValidate
			if err := s.git.ValidateAmbientRemote(cfg); err != nil {
				return err
			}
			phase = preparePhaseConfigureRemote
			if err := s.git.ConfigureRemoteNonInteractive(ctx, cfg); err != nil {
				return err
			}
			phase = preparePhaseFetch
			if err := s.git.FetchRefNonInteractive(ctx, cfg, cfg.FetchRef); err != nil {
				return err
			}
			phase = preparePhaseUpdateBranch
			return s.git.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef)
		}

		if cfg.RequiredCommit != "" {
			phase = preparePhaseClone
			preparedSource, err = s.git.PrepareSource(ctx, cfg, model.SourceRequirement{
				Ref:            cfg.Branch,
				RequiredCommit: cfg.RequiredCommit,
				Depth:          cfg.HistoryDepth,
			})
			if err != nil {
				return fail(err)
			}
			headOID, headRef = preparedSource.Commit, preparedSource.Ref
		} else {
			if cfg.PreparedGitDir {
				phase = preparePhaseValidate
				if err := s.git.ValidatePreparedGitDir(ctx, cfg); err != nil {
					return fail(err)
				}
				phase = preparePhaseFetch
				if err := s.git.FetchRefNonInteractive(ctx, cfg, cfg.FetchRef); err != nil {
					return fail(err)
				}
				phase = preparePhaseUpdateBranch
				if err := s.git.PrepareFetchedBranch(ctx, cfg, cfg.FetchRef); err != nil {
					return fail(err)
				}
			} else {
				phase = preparePhaseValidate
				if strings.TrimSpace(cfg.RemoteURL) == "" {
					return fail(errors.New("remote URL is required for async clone"))
				}
				if _, err := os.Stat(cfg.GitDir); err == nil {
					if err := prepareExistingClone(); err != nil {
						return fail(err)
					}
				} else if errors.Is(err, os.ErrNotExist) {
					phase = preparePhaseValidate
					if err := s.git.ValidateAmbientRemote(cfg); err != nil {
						return fail(err)
					}
					phase = preparePhaseClone
					if err := s.git.CloneBloblessNonInteractive(ctx, cfg); err != nil {
						return fail(err)
					}
					if !sameBranchRef(cfg.FetchRef, cfg.Branch) {
						if err := prepareExistingClone(); err != nil {
							return fail(err)
						}
					}
				} else {
					return fail(err)
				}
			}
			phase = preparePhaseResolveHEAD
			headOID, headRef, err = s.git.ResolveHEAD(ctx, cfg)
			if err != nil {
				return fail(err)
			}
			preparedSource = model.PreparedSource{Ref: headRef, Commit: headOID}
		}
		phase = preparePhaseOpenSnapshot
		snap, closeSnap, err := s.snapshotForPrepare(ctx, cfg)
		if err != nil {
			return fail(err)
		}
		if closeSnap {
			defer snap.Close()
		}
		prevGen := int64(0)
		phase = preparePhaseBuildTree
		var publishPhase string
		err = s.withRepoConfigLock(ctx, cfg.Name, func() error {
			current, err := s.configVersionIsCurrent(ctx, cfg)
			if err != nil {
				return err
			}
			if !current {
				return registry.ErrRepoChanged
			}
			_, _, prevGen, _ = snap.ReadState(ctx)
			gen, publishPhase, err = s.publishSnapshot(ctx, cfg, snap, headOID, headRef)
			return err
		})
		if err != nil {
			if errors.Is(err, registry.ErrRepoChanged) {
				return fmt.Errorf("prepare superseded by newer repo config: %w", err)
			}
			if publishPhase == snapshotPhasePublish {
				phase = preparePhasePublishSnapshot
			}
			return fail(err)
		}
		if err := s.recordAcquisition(ctx, cfg, preparedSource); err != nil {
			return fail(err)
		}
		phase = preparePhasePersistReady
		latest, err := s.registry.GetRepo(ctx, cfg.Name)
		if err != nil {
			return fail(err)
		}
		s.fillPaths(&latest)
		if strings.TrimSpace(latest.FetchRef) == "" {
			latest.FetchRef = latest.Branch
		}
		if !samePrepareConfig(cfg, latest) {
			return errors.New("prepare superseded by newer repo config")
		}
		if err := s.setPrepareStateBeforeReadyGate(ctx, cfg); err != nil {
			return fail(err)
		}
		phase = preparePhaseActivateRuntime
		if err := s.completePreparedRuntime(ctx, cfg, headOID, headRef, gen); err != nil {
			return fail(err)
		}
		phase = preparePhaseCleanupSnapshot
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		cleanupErr := s.withRepoConfigLock(cleanupCtx, cfg.Name, func() error {
			current, err := s.configVersionIsCurrent(cleanupCtx, cfg)
			if err != nil {
				return err
			}
			if !current {
				return registry.ErrRepoChanged
			}
			return snap.PruneGenerations(cleanupCtx, min(prevGen, gen-1))
		})
		if cleanupErr != nil && !errors.Is(cleanupErr, registry.ErrRepoChanged) {
			s.logger.Warn("snapshot generation cleanup failed", "repo", cfg.Name, "error", cleanupErr)
		}
		cleanupCancel()
		s.configureStatusOptimization(ctx, cfg)
		return nil
	})
	if retErr != nil && !lockAcquired {
		return fail(retErr)
	}
	return retErr
}

func defaultFetchRef(sourceRef string) string {
	if branch := branchRefName(sourceRef); branch != "" {
		return branch
	}
	return strings.TrimSpace(sourceRef)
}

func sameBranchRef(fetchRef string, branch string) bool {
	return canonicalFetchRef(fetchRef) == canonicalFetchRef(branch)
}

func canonicalFetchRef(ref string) string {
	if branch := branchRefName(ref); branch != "" {
		return "refs/heads/" + branch
	}
	return strings.TrimSpace(ref)
}

func branchRefName(ref string) string {
	ref = strings.TrimSpace(ref)
	for _, prefix := range []string{"refs/heads/", "refs/remotes/origin/", "origin/"} {
		if after, ok := strings.CutPrefix(ref, prefix); ok {
			return after
		}
	}
	if strings.HasPrefix(ref, "refs/") {
		return ""
	}
	return ref
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
	if rt != nil && !samePrepareConfig(rt.cfg, cfg) {
		s.mu.Unlock()
		return registry.ErrRepoChanged
	}
	s.mu.Unlock()
	if rt == nil {
		return nil
	}
	if !s.prepareConfigStillCurrent(ctx, cfg) {
		return registry.ErrRepoChanged
	}
	baseLookup := func(path string) (model.BaseNode, bool, error) {
		return rt.snapshot.LookupNode(ctx, gen, path)
	}
	commitTime := int64(0)
	if ts, err := s.git.CommitTimestamp(ctx, cfg, headOID); err == nil {
		commitTime = ts
	} else {
		s.logger.Warn("commit timestamp unavailable", "repo", cfg.Name, "error", err)
	}
	if err := rt.resolver.Transition(func() error {
		if err := rt.overlay.ReconcileChecked(ctx, baseLookup); err != nil {
			return err
		}
		rt.resolver.SetCommitTime(commitTime)
		rt.resolver.SetGeneration(gen)
		return nil
	}); err != nil {
		return err
	}
	if !s.prepareConfigStillCurrent(ctx, cfg) {
		return registry.ErrRepoChanged
	}
	s.mu.Lock()
	if s.running[cfg.ID] != rt || !samePrepareConfig(rt.cfg, cfg) {
		s.mu.Unlock()
		return registry.ErrRepoChanged
	}
	rt.cfg = cfg
	rt.cfg.PrepareState = model.PrepareStateReady
	rt.cfg.PrepareError = ""
	setHeadState(&rt.state, headOID, headRef, gen)
	rt.state.State = repoStateMounted
	rt.state.PrepareError = ""
	s.mu.Unlock()
	rt.gate.MarkReady()
	s.startRepoBackground(rt)
	return nil
}

func (s *Service) setPrepareState(ctx context.Context, cfg model.RepoConfig, state string, stateErr error) error {
	return s.applyPrepareState(ctx, cfg, state, stateErr, true)
}

func (s *Service) setPrepareStateBeforeReadyGate(ctx context.Context, cfg model.RepoConfig) error {
	return s.applyPrepareState(ctx, cfg, model.PrepareStateReady, nil, false)
}

func (s *Service) applyPrepareState(ctx context.Context, cfg model.RepoConfig, state string, stateErr error, applyReadyRuntime bool) error {
	msg := ""
	if stateErr != nil {
		msg = auth.RedactLogString(stateErr.Error(), cfg.RemoteURL)
	}
	if err := s.registry.UpdatePrepareStateForConfig(ctx, cfg, state, msg); err != nil {
		return err
	}
	s.mu.Lock()
	if rt, ok := s.running[cfg.ID]; ok {
		if !samePrepareConfig(rt.cfg, cfg) {
			s.mu.Unlock()
			return nil
		}
		rt.cfg.PrepareState = state
		rt.cfg.PrepareError = msg
		rt.state.PrepareError = msg
		if state != model.PrepareStateReady || applyReadyRuntime {
			rt.state.State = runtimeStateForPrepareState(state)
		}
		if rt.gate != nil {
			switch state {
			case model.PrepareStateFailed:
				rt.gate.MarkFailed(prepareGateError(msg))
			case model.PrepareStateReady:
				if applyReadyRuntime {
					rt.gate.MarkReady()
				}
			default:
				rt.gate.Reset()
			}
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) onHEADChanged(ctx context.Context, rt *repoRuntime) {
	rt.headMu.Lock()
	defer rt.headMu.Unlock()
	s.mu.Lock()
	if s.running[rt.cfg.ID] != rt {
		s.mu.Unlock()
		return
	}
	cfg := rt.cfg
	s.mu.Unlock()
	if cfg.RequiredCommit != "" {
		return
	}
	oid, ref, err := s.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		s.logger.Error("HEAD resolve failed", "repo", cfg.Name, "error", err)
		return
	}
	s.mu.Lock()
	prevOID := rt.state.CurrentHEADOID
	prevRef := rt.state.CurrentHEADRef
	prevGen := rt.state.SnapshotGeneration
	s.mu.Unlock()
	if oid == prevOID {
		if ref == prevRef {
			return
		}
		if err := rt.snapshot.UpdateHEADRef(ctx, ref); err != nil {
			s.logger.Warn("snapshot head_ref update failed", "repo", cfg.Name, "error", err)
		}
		s.mu.Lock()
		rt.state.CurrentHEADRef = ref
		s.mu.Unlock()
		return
	}
	storedOID, storedRef, gen, stateErr := rt.snapshot.ReadState(ctx)
	if stateErr != nil || storedOID != oid || storedRef != ref || gen == 0 {
		var phase string
		gen, phase, err = s.publishSnapshot(ctx, cfg, rt.snapshot, oid, ref)
		if err != nil {
			msg := "tree rebuild failed"
			if phase == snapshotPhasePublish {
				msg = "snapshot publish failed"
			}
			s.logger.Error(msg, "repo", cfg.Name, "error", err)
			return
		}
	}
	baseLookup := func(path string) (model.BaseNode, bool, error) {
		return rt.snapshot.LookupNode(ctx, gen, path)
	}
	commitTime := int64(0)
	if ts, timestampErr := s.git.CommitTimestamp(ctx, cfg, oid); timestampErr == nil {
		commitTime = ts
	} else if ctx.Err() == nil {
		s.logger.Warn("commit timestamp unavailable", "repo", cfg.Name, "error", timestampErr)
	}
	err = rt.resolver.Transition(func() error {
		if err := rt.overlay.ReconcileChecked(ctx, baseLookup); err != nil {
			return err
		}
		rt.resolver.SetCommitTime(commitTime)
		rt.resolver.SetGeneration(gen)
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("overlay reconcile failed", "repo", cfg.Name, "error", err)
		s.scheduleHEADRetry(rt)
		return
	}

	s.mu.Lock()
	if s.running[cfg.ID] == rt {
		setHeadState(&rt.state, oid, ref, gen)
	}
	s.mu.Unlock()
	if err := rt.snapshot.PruneGenerations(ctx, min(prevGen, gen-1)); err != nil {
		s.logger.Warn("snapshot generation cleanup failed", "repo", cfg.Name, "error", err)
	}
}

func (s *Service) scheduleHEADRetry(rt *repoRuntime) {
	s.mu.Lock()
	if rt.stopping {
		s.mu.Unlock()
		return
	}
	rt.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer rt.workers.Done()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-rt.ctx.Done():
			return
		case <-timer.C:
			s.onHEADChanged(rt.ctx, rt)
		}
	}()
}

func (s *Service) configureStatusOptimization(ctx context.Context, cfg model.RepoConfig) {
	if err := s.git.ConfigureStatusOptimization(ctx, cfg, s.root); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Warn("git status optimization setup failed", "repo", cfg.Name, "error", err)
	}
}

func (s *Service) FSMonitorHook(ctx context.Context, name string, w io.Writer) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	s.fillPaths(&cfg)
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer ov.Close()
	entries, err := ov.ListAll(ctx)
	if err != nil {
		return err
	}
	paths := fsMonitorDirtyPaths(entries)
	token := fmt.Sprintf("artifact-fs:%s:%d", cfg.ID, time.Now().UnixNano())
	if err := writeHookField(w, token); err != nil {
		return err
	}
	for _, p := range paths {
		if err := writeHookField(w, p); err != nil {
			return err
		}
	}
	return nil
}

func writeHookField(w io.Writer, value string) error {
	n, err := io.WriteString(w, value)
	if err != nil {
		return err
	}
	if n != len(value) {
		return io.ErrShortWrite
	}
	n, err = w.Write([]byte{0})
	if err != nil {
		return err
	}
	if n != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func fsMonitorDirtyPaths(entries []model.OverlayEntry) []string {
	set := map[string]struct{}{}
	add := func(path string) {
		path = model.CleanPath(path)
		if path == "." {
			return
		}
		set[path] = struct{}{}
	}
	for _, e := range entries {
		add(e.Path)
		if e.Kind == model.OverlayKindRename && e.TargetPath != "" {
			add(e.TargetPath)
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (s *Service) refreshLoop(rt *repoRuntime) {
	s.mu.Lock()
	baseInterval := rt.cfg.RefreshInterval
	s.mu.Unlock()
	if baseInterval <= 0 {
		baseInterval = 30 * time.Second
	}
	backoff := baseInterval
	ticker := time.NewTicker(backoff)
	defer ticker.Stop()
	for {
		select {
		case <-rt.ctx.Done():
			return
		case interval := <-rt.refresh:
			if interval <= 0 {
				continue
			}
			baseInterval = interval
			backoff = interval
			ticker.Reset(backoff)
		case <-ticker.C:
			s.mu.Lock()
			cfg := rt.cfg
			s.mu.Unlock()
			if cfg.RemoteRefreshDisabled {
				continue
			}
			ctx, cancel := context.WithTimeout(rt.ctx, 30*time.Second)
			err := s.git.Fetch(ctx, cfg)
			if err != nil {
				s.mu.Lock()
				markFetchFailure(&rt.state, auth.RedactString(err.Error()))
				s.mu.Unlock()
				cancel()
				// Exponential backoff on failure, capped at maxBackoff
				maxBackoff := max(10*time.Minute, baseInterval)
				if backoff >= maxBackoff/2 {
					backoff = maxBackoff
				} else {
					backoff *= 2
				}
				ticker.Reset(backoff)
				continue
			}
			state, abErr := s.fetchState(ctx, cfg)
			cancel()
			// Reset backoff on success
			backoff = baseInterval
			ticker.Reset(backoff)
			s.mu.Lock()
			markFetchResult(&rt.state, time.Now(), "ok", rt.mfs != nil)
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
	if cfg.PrepareState == model.PrepareStateSyncPreparing {
		st.State = model.PrepareStatePreparing
	} else if isPendingOrFailedPrepareState(cfg.PrepareState) {
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
		// Verified acquisition itself writes FETCH_HEAD before its receipt.
		// Only a newer mtime is evidence of a later remote refresh.
		if cfg.RequiredCommit == "" || (!cfg.AcquiredAt.IsZero() && fi.ModTime().After(cfg.AcquiredAt)) {
			st.LastFetchAt = fi.ModTime()
			st.LastFetchResult = "ok"
		}
	}
	applyHydrationStats(&st, cfg.BlobCacheDir)
	applySourceStatus(&st, cfg)
	return st
}

func applySourceStatus(st *model.RepoRuntimeState, cfg model.RepoConfig) {
	st.SourceRef = cfg.Branch
	st.RequiredCommit = cfg.RequiredCommit
	st.RemoteRefreshDisabled = cfg.RemoteRefreshDisabled
	switch {
	case cfg.RequiredCommit == "":
		st.Acquisition = "not_required"
	case cfg.AcquiredRef == cfg.Branch && strings.EqualFold(cfg.AcquiredCommit, cfg.RequiredCommit) && !cfg.AcquiredAt.IsZero():
		st.Acquisition = "verified"
	default:
		st.Acquisition = "pending"
	}
}

func (s *Service) publishSnapshot(ctx context.Context, cfg model.RepoConfig, snap *snapshot.Store, oid string, ref string) (int64, string, error) {
	nodes, err := s.git.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		return 0, snapshotPhaseBuild, err
	}
	gen, err := snap.PublishGeneration(ctx, oid, ref, nodes)
	if err != nil {
		return 0, snapshotPhasePublish, err
	}
	return gen, "", nil
}

type sizeUpdateBatcher struct {
	snapshot *snapshot.Store
	logger   *slog.Logger
	repoName string
	interval time.Duration
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	pending  map[int64]map[string]int64
	stopped  bool
}

func newSizeUpdateBatcher(snap *snapshot.Store, logger *slog.Logger, repoName string) *sizeUpdateBatcher {
	return &sizeUpdateBatcher{
		snapshot: snap,
		logger:   logger,
		repoName: repoName,
		interval: sizeUpdateFlushInterval,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		pending:  map[int64]map[string]int64{},
	}
}

func (b *sizeUpdateBatcher) Start(ctx context.Context) {
	go b.run(ctx)
}

func (b *sizeUpdateBatcher) Add(generation int64, objectOID string, size int64) {
	if generation <= 0 || strings.TrimSpace(objectOID) == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	if b.pending[generation] == nil {
		b.pending[generation] = map[string]int64{}
	}
	b.pending[generation][objectOID] = size
}

func (b *sizeUpdateBatcher) Stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.stopped = true
		b.mu.Unlock()
		close(b.stopCh)
		<-b.done
		b.Flush()
	})
}

func (b *sizeUpdateBatcher) run(ctx context.Context) {
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	defer close(b.done)
	defer b.Flush()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stopCh:
			return
		case <-ticker.C:
			b.Flush()
		}
	}
}

func (b *sizeUpdateBatcher) Flush() {
	b.mu.Lock()
	pending := b.pending
	b.pending = map[int64]map[string]int64{}
	b.mu.Unlock()
	for gen, sizes := range pending {
		if err := b.snapshot.UpdateSizes(context.Background(), gen, sizes); err != nil && b.logger != nil {
			b.logger.Warn("snapshot size backfill failed", "repo", b.repoName, "generation", gen, "error", err)
		}
	}
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

	s.joinRuntimeMount(rt)
}

func (s *Service) joinRuntimeMount(rt *repoRuntime) {
	s.mu.Lock()
	mfs := rt.mfs
	if mfs == nil || rt.joinDone != nil {
		s.mu.Unlock()
		return
	}
	done := make(chan struct{})
	rt.joinDone = done
	s.mu.Unlock()
	go func() {
		_ = mfs.Join(context.Background())
		close(done)
		s.mu.Lock()
		if !rt.stopping && !rt.detached && rt.mfs == mfs {
			rt.mfs = nil
			rt.joinDone = nil
			rt.state.State = repoStateDegraded
		}
		s.mu.Unlock()
	}()
}

func (s *Service) retryRuntimeMount(rt *repoRuntime) {
	s.mu.Lock()
	if rt == nil || rt.mfs != nil || rt.engine == nil || rt.stopping {
		s.mu.Unlock()
		return
	}
	mf := s.mountFailures[rt.cfg.ID]
	if mf != nil && time.Since(mf.lastAttempt) < mf.backoff {
		s.mu.Unlock()
		return
	}
	cfg, resolver, engine, gate := rt.cfg, rt.resolver, rt.engine, rt.gate
	rt.mounts.Add(1)
	s.mu.Unlock()
	defer rt.mounts.Done()

	mfs, err := fusefs.MountRepoWithGate(cfg, resolver, engine, gate)
	s.mu.Lock()
	if err != nil {
		mf = s.mountFailures[cfg.ID]
		if mf == nil {
			mf = &mountFailure{}
			s.mountFailures[cfg.ID] = mf
		}
		mf.lastAttempt = time.Now()
		if mf.backoff == 0 {
			mf.backoff = 30 * time.Second
		} else {
			mf.backoff = min(mf.backoff*2, 5*time.Minute)
		}
		s.mu.Unlock()
		return
	}
	if s.running[cfg.ID] != rt || rt.mfs != nil {
		s.mu.Unlock()
		_ = mfs.Unmount()
		_ = mfs.Join(context.Background())
		return
	}
	rt.mfs = mfs
	delete(s.mountFailures, cfg.ID)
	stopping := rt.stopping || rt.ctx.Err() != nil
	if !stopping && rt.state.State == repoStateDegraded {
		rt.state.State = repoStateMounted
	}
	s.mu.Unlock()
	if !stopping {
		s.configureStatusOptimization(rt.ctx, cfg)
	}
	s.joinRuntimeMount(rt)
}

func (s *Service) startRepoBackground(rt *repoRuntime) {
	s.mu.Lock()
	if rt.active || rt.stopping {
		s.mu.Unlock()
		return
	}
	rt.active = true
	gitDir := rt.cfg.GitDir
	fixedBase := rt.cfg.RequiredCommit != ""
	workerCount := 2
	if fixedBase {
		workerCount = 1
	}
	rt.workers.Add(workerCount)
	s.mu.Unlock()

	go func() {
		defer rt.workers.Done()
		s.refreshLoop(rt)
	}()

	if !fixedBase {
		w := watcher.New(500 * time.Millisecond)
		go func() {
			defer rt.workers.Done()
			w.Watch(rt.ctx, gitDir, func() {
				s.onHEADChanged(rt.ctx, rt)
			})
		}()
	}
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

func markFetchSuccess(st *model.RepoRuntimeState, at time.Time, state aheadBehind, mounted bool) {
	markFetchResult(st, at, "ok", mounted)
	applyAheadBehind(st, state)
}

func markFetchResult(st *model.RepoRuntimeState, at time.Time, result string, mounted bool) {
	st.LastFetchResult = result
	st.LastFetchAt = at
	if st.State == repoStateDegraded && result == "ok" && mounted {
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
		if !isObjectOIDName(d.Name()) {
			return nil
		}
		count++
		bytes += info.Size()
		return nil
	})
	return count, bytes
}

func isObjectOIDName(name string) bool {
	if len(name) != 40 && len(name) != 64 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func (s *Service) unmount(id model.RepoID) error {
	s.mu.Lock()
	rt, ok := s.running[id]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	if rt.stopping {
		s.mu.Unlock()
		return errors.New("runtime is already stopping")
	}
	rt.stopping = true
	s.mu.Unlock()
	if err := s.stopRuntime(rt); err != nil {
		s.mu.Lock()
		rt.stopping = false
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	delete(s.running, id)
	s.mu.Unlock()
	return nil
}

func (s *Service) stopRuntime(rt *repoRuntime) error {
	rt.mounts.Wait()
	s.mu.Lock()
	mfs := rt.mfs
	detached := rt.detached
	s.mu.Unlock()
	if mfs != nil && !detached {
		if err := mfs.Unmount(); err != nil {
			return fmt.Errorf("unmount %s: %w", rt.cfg.Name, err)
		}
		s.mu.Lock()
		rt.detached = true
		s.mu.Unlock()
	}
	if rt.cancel != nil {
		rt.cancel()
	}
	if rt.gate != nil {
		rt.gate.MarkFailed(context.Canceled)
	}
	rt.workers.Wait()
	s.joinRuntimeMount(rt)
	s.mu.Lock()
	joinDone := rt.joinDone
	s.mu.Unlock()
	if joinDone != nil {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-joinDone:
		case <-timer.C:
			return fmt.Errorf("timed out draining unmounted repo %s", rt.cfg.Name)
		}
	}
	if rt.hydrator != nil {
		rt.hydrator.Stop()
	}
	if rt.sizes != nil {
		rt.sizes.Stop()
	}
	if rt.snapshot != nil {
		_ = rt.snapshot.Close()
	}
	if rt.overlay != nil {
		_ = rt.overlay.Close()
	}
	return nil
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
	if strings.EqualFold(strings.TrimSpace(v), "never") {
		return 0, nil
	}
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
	if cfg.PreparedGitDir {
		return true
	}
	switch strings.TrimSpace(cfg.PrepareState) {
	case model.PrepareStatePreparing, model.PrepareStateReady, model.PrepareStateFailed:
		return true
	default:
		return false
	}
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
