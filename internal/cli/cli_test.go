package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestFormatStatusLineUsesNeverForUnsetFetch(t *testing.T) {
	st := model.RepoRuntimeState{
		RepoID:            "workerd",
		State:             "mounted",
		CurrentHEADOID:    "abc123",
		CurrentHEADRef:    "main",
		LastFetchResult:   "never",
		HydratedBlobCount: 3,
		HydratedBlobBytes: 42,
	}

	got := formatStatusLine(st)
	for _, want := range []string{
		"last_fetch=never",
		"result=never",
		"prepare_error=none",
		"hydrated_blobs=3",
		"hydrated_bytes=42",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status line %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "0001-01-01T00:00:00Z") {
		t.Fatalf("status line leaked zero time: %q", got)
	}
}

func TestFormatStatusLineFormatsFetchTimestamp(t *testing.T) {
	at := time.Date(2026, time.March, 31, 12, 34, 56, 0, time.UTC)
	st := model.RepoRuntimeState{LastFetchAt: at, LastFetchResult: "ok"}

	got := formatStatusLine(st)
	if !strings.Contains(got, "last_fetch=2026-03-31T12:34:56Z") {
		t.Fatalf("status line %q missing formatted timestamp", got)
	}
}

func TestFormatStatusLineKeepsPrepareErrorSingleLine(t *testing.T) {
	st := model.RepoRuntimeState{PrepareError: "fatal: clone failed\ntry again\tlater"}

	got := formatStatusLine(st)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("status line contains raw whitespace: %q", got)
	}
	if !strings.Contains(got, "prepare_error=fatal:_clone_failed_try_again_later") {
		t.Fatalf("status line %q missing normalized prepare error", got)
	}
}

func TestFormatStatusLineRedactsPrepareError(t *testing.T) {
	st := model.RepoRuntimeState{PrepareError: "clone https://token@example.com/org/repo.git?access_token=secret failed"}

	got := formatStatusLine(st)
	if strings.Contains(got, "token@example.com") || strings.Contains(got, "secret") {
		t.Fatalf("status line leaked prepare error credential: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("status line %q missing redaction marker", got)
	}
}

func TestAddRepoAsyncCLIRegistersWithoutClone(t *testing.T) {
	t.Setenv("ARTIFACT_FS_ROOT", t.TempDir())
	var stdout, stderr bytes.Buffer

	code := Run(context.Background(), []string{
		"add-repo",
		"--name", "repo",
		"--remote", "https://github.com/example/repo.git",
		"--branch", "main",
		"--async",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run exit = %d, stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "queued repo\n" {
		t.Fatalf("stdout = %q, want queued repo", got)
	}
}

func TestAddRepoAsyncCLIFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "prepared_gitdir_requires_async",
			args: []string{"add-repo", "--name", "repo", "--prepared-gitdir", "--git-dir", "/tmp/repo.git"},
			want: "--prepared-gitdir requires --async",
		},
		{
			name: "prepared_gitdir_requires_git_dir",
			args: []string{"add-repo", "--name", "repo", "--async", "--prepared-gitdir"},
			want: "--git-dir is required with --prepared-gitdir",
		},
		{
			name: "async_clone_requires_remote",
			args: []string{"add-repo", "--name", "repo", "--async"},
			want: "--remote is required",
		},
		{
			name: "async_rejects_inline_credentials",
			args: []string{"add-repo", "--name", "repo", "--async", "--remote", "https://token@example.com/org/repo.git"},
			want: "async repositories must use ambient credentials",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ARTIFACT_FS_ROOT", t.TempDir())
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), tt.args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("Run unexpectedly succeeded, stdout=%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestPrepareCLIReportsQueuedPrepare(t *testing.T) {
	t.Setenv("ARTIFACT_FS_ROOT", t.TempDir())
	var stdout, stderr bytes.Buffer
	ctx := context.Background()
	code := Run(ctx, []string{
		"add-repo",
		"--name", "repo",
		"--remote", "https://github.com/example/repo.git",
		"--async",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("add-repo exit = %d, stderr=%q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run(ctx, []string{"prepare", "--name", "repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("prepare exit = %d, stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "preparing repo\n" {
		t.Fatalf("stdout = %q, want preparing repo", got)
	}
}
