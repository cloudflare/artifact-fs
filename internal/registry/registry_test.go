package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestRepoPrepareFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, filepath.Join(t.TempDir(), "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := model.RepoConfig{
		ID:                    "repo",
		Name:                  "repo",
		MountRoot:             "/mnt",
		MountPath:             "/mnt/repo",
		RemoteURL:             "https://github.com/example/repo.git",
		RemoteURLRedacted:     "https://github.com/example/repo.git",
		Branch:                "master",
		RefreshInterval:       time.Minute,
		GitDir:                "/git/repo",
		OverlayDir:            "/overlay/repo",
		BlobCacheDir:          "/cache/repo",
		MetaDBPath:            "/meta/repo.sqlite",
		OverlayDBPath:         "/overlay/repo/meta.sqlite",
		Enabled:               true,
		PreparedGitDir:        true,
		FetchRef:              "master",
		PrepareState:          model.PrepareStatePreparing,
		RequiredCommit:        "0123456789012345678901234567890123456789",
		HistoryDepth:          1,
		RemoteRefreshDisabled: true,
		ConfigVersion:         "version-1",
	}
	if err := store.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePrepareState(ctx, cfg.ID, model.PrepareStateFailed, "clone failed"); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if !got.PreparedGitDir {
		t.Fatal("PreparedGitDir = false, want true")
	}
	if got.FetchRef != "master" {
		t.Fatalf("FetchRef = %q, want master", got.FetchRef)
	}
	if got.PrepareState != model.PrepareStateFailed {
		t.Fatalf("PrepareState = %q, want failed", got.PrepareState)
	}
	if got.PrepareError != "clone failed" {
		t.Fatalf("PrepareError = %q, want clone failed", got.PrepareError)
	}
	if got.RequiredCommit != cfg.RequiredCommit || got.HistoryDepth != 1 || !got.RemoteRefreshDisabled {
		t.Fatalf("source policy = %+v", got)
	}
	if err := store.RecordAcquisition(ctx, cfg, model.PreparedSource{Ref: cfg.Branch, Commit: cfg.RequiredCommit, Verified: true}); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetRepo(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.AcquiredRef != cfg.Branch || got.AcquiredCommit != cfg.RequiredCommit || got.AcquiredAt.IsZero() {
		t.Fatalf("acquisition receipt = %+v", got)
	}
	if got.ConfigVersion != "version-1" {
		t.Fatalf("ConfigVersion = %q, want version-1", got.ConfigVersion)
	}
}

func TestPrepareStateUpdateRejectsOlderConfigVersion(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, filepath.Join(t.TempDir(), "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := model.RepoConfig{ID: "repo", Name: "repo", Branch: "main", ConfigVersion: "version-1"}
	if err := store.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	stale := cfg
	cfg.ConfigVersion = "version-2"
	cfg.PrepareState = model.PrepareStatePreparing
	if err := store.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePrepareStateForConfig(ctx, stale, "", "old failure"); err != ErrRepoChanged {
		t.Fatalf("stale update error = %v, want ErrRepoChanged", err)
	}
	got, err := store.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigVersion != "version-2" || got.PrepareState != model.PrepareStatePreparing || got.PrepareError != "" {
		t.Fatalf("new config changed: version/state/error = %q/%q/%q", got.ConfigVersion, got.PrepareState, got.PrepareError)
	}
}

func TestSubsecondRefreshRoundTripsWithoutChangingPrepareState(t *testing.T) {
	ctx := context.Background()
	store, err := New(ctx, filepath.Join(t.TempDir(), "repos.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := model.RepoConfig{ID: "repo", Name: "repo", Branch: "main", RefreshInterval: time.Second, PrepareState: model.PrepareStatePreparing}
	if err := store.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRefresh(ctx, cfg.ID, 250*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshInterval != 250*time.Millisecond {
		t.Fatalf("refresh interval = %v, want 250ms", got.RefreshInterval)
	}
	if got.PrepareState != model.PrepareStatePreparing {
		t.Fatalf("prepare state = %q, want preparing", got.PrepareState)
	}
}
