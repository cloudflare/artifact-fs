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
		ID:                "repo",
		Name:              "repo",
		MountRoot:         "/mnt",
		MountPath:         "/mnt/repo",
		RemoteURL:         "https://github.com/example/repo.git",
		RemoteURLRedacted: "https://github.com/example/repo.git",
		Branch:            "master",
		RefreshInterval:   time.Minute,
		GitDir:            "/git/repo",
		OverlayDir:        "/overlay/repo",
		BlobCacheDir:      "/cache/repo",
		MetaDBPath:        "/meta/repo.sqlite",
		OverlayDBPath:     "/overlay/repo/meta.sqlite",
		Enabled:           true,
		PreparedGitDir:    true,
		FetchRef:          "master",
		PrepareState:      model.PrepareStatePreparing,
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
}
