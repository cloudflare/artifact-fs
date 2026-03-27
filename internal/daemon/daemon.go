package daemon

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/registry"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
	"github.com/cloudflare/artifact-fs/internal/watcher"
)

type Service struct {
	root      string
	mountRoot string
	logger    *slog.Logger
	registry  *registry.Store
	git       *gitstore.Store
	mu        sync.Mutex
	running   map[model.RepoID]*repoRuntime
}

type repoRuntime struct {
	cfg      model.RepoConfig
	snapshot *snapshot.Store
	overlay  *overlay.Store
	hydrator *hydrator.Service
	resolver *fusefs.Resolver
	mfs      fusefs.MountedFS
	state    model.RepoRuntimeState
	stop     chan struct{}
}

func New(ctx context.Context, root string, logger *slog.Logger) (*Service, error) {
	reg, err := registry.New(ctx, filepath.Join(root, "config", "repos.sqlite"))
	if err != nil {
		return nil, err
	}
	return &Service{
		root:     root,
		logger:   logger,
		registry: reg,
		git:      gitstore.New(logger),
		running:  map[model.RepoID]*repoRuntime{},
	}, nil
}

func (s *Service) SetMountRoot(root string) {
	if strings.TrimSpace(root) != "" {
		s.mountRoot = root
	}
}

func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rt := range s.running {
		s.stopRuntime(rt)
		delete(s.running, id)
	}
	return s.registry.Close()
}

func (s *Service) Start(ctx context.Context) error {
	repos, err := s.registry.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if !repo.Enabled {
			continue
		}
		if err := s.mountRepo(ctx, repo); err != nil {
			s.logger.Error("repo mount failed", "repo", repo.Name, "error", err)
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *Service) AddRepo(ctx context.Context, cfg model.RepoConfig) error {
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
	if err := s.registry.AddRepo(ctx, cfg); err != nil {
		return err
	}
	// Clone and build snapshot so the repo is ready to mount, but don't start
	// the FUSE server -- that's the daemon's job.
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
	s.mu.Lock()
	rt, ok := s.running[cfg.ID]
	s.mu.Unlock()
	if !ok {
		return model.RepoRuntimeState{RepoID: cfg.ID, State: "unmounted"}, nil
	}
	dirty, _ := rt.overlay.DirtyCount(ctx)
	rt.state.DirtyOverlay = dirty > 0
	return rt.state, nil
}

func (s *Service) FetchNow(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	if err := s.git.Fetch(ctx, cfg); err != nil {
		return err
	}
	ahead, behind, diverged, err := s.git.ComputeAheadBehind(ctx, cfg)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if rt, ok := s.running[cfg.ID]; ok {
		rt.state.AheadCount = ahead
		rt.state.BehindCount = behind
		rt.state.Diverged = diverged
		rt.state.LastFetchAt = time.Now()
		rt.state.LastFetchResult = "ok"
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) Remount(ctx context.Context, name string) error {
	cfg, err := s.registry.GetRepo(ctx, name)
	if err != nil {
		return err
	}
	s.unmount(cfg.ID)
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
	if err := os.MkdirAll(cfg.MountPath, 0o755); err != nil {
		return err
	}
	if err := s.git.CloneBlobless(ctx, cfg); err != nil {
		return err
	}
	headOID, _, err := s.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		return err
	}
	nodes, err := s.git.BuildTreeIndex(ctx, cfg, headOID)
	if err != nil {
		return err
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		return err
	}
	defer snap.Close()
	_, err = snap.PublishGeneration(ctx, cfg.ID, headOID, "", nodes)
	return err
}

// mountRepo opens all stores, starts the FUSE server, watcher, and refresh
// loop. Called by the daemon's Start for each registered repo.
func (s *Service) mountRepo(ctx context.Context, cfg model.RepoConfig) error {
	if err := os.MkdirAll(cfg.MountPath, 0o755); err != nil {
		return err
	}
	// Clone if not already present (idempotent)
	if err := s.git.CloneBlobless(ctx, cfg); err != nil {
		return err
	}
	headOID, headRef, err := s.git.ResolveHEAD(ctx, cfg)
	if err != nil {
		return err
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		return err
	}
	gen, err := snap.CurrentGeneration(ctx)
	if err != nil || gen == 0 {
		nodes, bErr := s.git.BuildTreeIndex(ctx, cfg, headOID)
		if bErr != nil {
			return bErr
		}
		gen, err = snap.PublishGeneration(ctx, cfg.ID, headOID, headRef, nodes)
		if err != nil {
			return err
		}
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		return err
	}
	h := hydrator.New(s.git)
	h.Start(2, cfg)

	resolver := &fusefs.Resolver{
		RepoID:   cfg.ID,
		Snapshot: snap,
		Overlay:  ov,
	}
	resolver.SetGeneration(gen)
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

	rt := &repoRuntime{
		cfg:      cfg,
		snapshot: snap,
		overlay:  ov,
		hydrator: h,
		resolver: resolver,
		mfs:      mfs,
		state: model.RepoRuntimeState{
			RepoID:             cfg.ID,
			CurrentHEADOID:     headOID,
			CurrentHEADRef:     headRef,
			SnapshotGeneration: gen,
			State:              "ready",
		},
		stop: make(chan struct{}),
	}
	s.mu.Lock()
	s.running[cfg.ID] = rt
	s.mu.Unlock()

	go s.refreshLoop(rt)

	w := watcher.New(500 * time.Millisecond)
	go w.Watch(ctx, cfg.GitDir, func(sig watcher.Signal) {
		if sig.HEADChanged {
			s.onHEADChanged(ctx, rt)
		}
	})

	if mfs != nil {
		go func() {
			_ = mfs.Join(ctx)
		}()
	}

	return nil
}

func (s *Service) onHEADChanged(ctx context.Context, rt *repoRuntime) {
	oid, ref, err := s.git.ResolveHEAD(ctx, rt.cfg)
	if err != nil {
		s.logger.Error("HEAD resolve failed", "repo", rt.cfg.Name, "error", err)
		return
	}
	if oid == rt.state.CurrentHEADOID {
		return
	}
	nodes, err := s.git.BuildTreeIndex(ctx, rt.cfg, oid)
	if err != nil {
		s.logger.Error("tree rebuild failed", "repo", rt.cfg.Name, "error", err)
		return
	}
	gen, err := rt.snapshot.PublishGeneration(ctx, rt.cfg.ID, oid, ref, nodes)
	if err != nil {
		s.logger.Error("snapshot publish failed", "repo", rt.cfg.Name, "error", err)
		return
	}
	_ = rt.overlay.Reconcile(ctx, gen)
	// Atomically update the resolver's generation so FUSE ops see the new snapshot
	rt.resolver.SetGeneration(gen)
	s.mu.Lock()
	rt.state.CurrentHEADOID = oid
	rt.state.CurrentHEADRef = ref
	rt.state.SnapshotGeneration = gen
	s.mu.Unlock()
}

func (s *Service) refreshLoop(rt *repoRuntime) {
	backoff := rt.cfg.RefreshInterval
	const maxBackoff = 10 * time.Minute
	ticker := time.NewTicker(backoff)
	defer ticker.Stop()
	for {
		select {
		case <-rt.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := s.git.Fetch(ctx, rt.cfg)
			if err != nil {
				s.mu.Lock()
				rt.state.State = "degraded"
				rt.state.LastFetchResult = auth.RedactString(err.Error())
				s.mu.Unlock()
				cancel()
				// Exponential backoff on failure, capped at maxBackoff
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				ticker.Reset(backoff)
				continue
			}
			ahead, behind, diverged, abErr := s.git.ComputeAheadBehind(ctx, rt.cfg)
			cancel()
			// Reset backoff on success
			backoff = rt.cfg.RefreshInterval
			ticker.Reset(backoff)
			s.mu.Lock()
			rt.state.LastFetchResult = "ok"
			rt.state.LastFetchAt = time.Now()
			if rt.state.State == "degraded" {
				rt.state.State = "ready"
			}
			if abErr == nil {
				rt.state.AheadCount = ahead
				rt.state.BehindCount = behind
				rt.state.Diverged = diverged
			}
			s.mu.Unlock()
		}
	}
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
	close(rt.stop)
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
