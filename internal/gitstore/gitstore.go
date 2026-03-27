package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type Store struct {
	logger *slog.Logger
}

func New(logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{logger: logger}
}

func (s *Store) CloneBlobless(ctx context.Context, cfg model.RepoConfig) error {
	if _, err := os.Stat(cfg.GitDir); err == nil {
		return nil
	}
	parent := filepath.Dir(cfg.GitDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	// Use a unique temp dir to avoid races between concurrent clones.
	target, err := os.MkdirTemp(parent, ".clone-*")
	if err != nil {
		return fmt.Errorf("mktemp clone dir: %w", err)
	}
	defer os.RemoveAll(target)

	// Strip credentials from the CLI-visible URL; pass them via a credential helper
	// so they don't appear in ps output.
	safeURL, credHelper := credentialEnv(cfg.RemoteURL)

	args := []string{"clone", "--filter=blob:none", "--no-checkout", "--single-branch", "--branch", cfg.Branch, safeURL, target}
	if _, err := runGitWithEnv(ctx, "", credHelper, args...); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(target, ".git"), cfg.GitDir); err != nil {
		return err
	}
	// Populate the index so git status works inside the mount.
	if _, err := runGit(ctx, cfg.GitDir, "read-tree", "HEAD"); err != nil {
		return err
	}
	return nil
}

func (s *Store) Fetch(ctx context.Context, repo model.RepoConfig) error {
	_, err := runGit(ctx, repo.GitDir, "fetch", "origin")
	return err
}

func (s *Store) ResolveHEAD(ctx context.Context, repo model.RepoConfig) (oid string, ref string, err error) {
	oid, err = runGit(ctx, repo.GitDir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	ref, err = runGit(ctx, repo.GitDir, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		ref = "DETACHED"
		err = nil
	}
	return strings.TrimSpace(oid), strings.TrimSpace(ref), nil
}

func (s *Store) BuildTreeIndex(ctx context.Context, repo model.RepoConfig, headOID string) ([]model.BaseNode, error) {
	out, err := runGit(ctx, repo.GitDir, "ls-tree", "-r", "-t", headOID)
	if err != nil {
		return nil, err
	}
	scan := bufio.NewScanner(strings.NewReader(out))
	nodes := []model.BaseNode{rootNode(repo.ID)}
	var blobOIDs []string
	blobIndex := map[string][]int{} // oid -> indices into nodes
	for scan.Scan() {
		line := scan.Text()
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		meta := strings.Fields(parts[0])
		if len(meta) < 3 {
			continue
		}
		modeStr := meta[0]
		typ := meta[1]
		oid := meta[2]
		path := parts[1]
		mode64, _ := strconv.ParseUint(modeStr, 8, 32)
		mode := uint32(mode64)

		nodeType := normalizeGitType(typ, mode)
		if typ == "commit" {
			continue
		}

		n := model.BaseNode{
			RepoID:    repo.ID,
			Path:      path,
			Type:      nodeType,
			Mode:      mode,
			ObjectOID: oid,
			SizeState: "unknown",
			SizeBytes: 0,
		}
		idx := len(nodes)
		nodes = append(nodes, n)
		if typ == "blob" && oid != "" {
			blobIndex[oid] = append(blobIndex[oid], idx)
			if len(blobIndex[oid]) == 1 {
				blobOIDs = append(blobOIDs, oid)
			}
		}
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	// Batch-resolve sizes using cat-file --batch-check. This reads from local
	// pack metadata and doesn't trigger network fetches on blobless clones.
	if err := s.batchResolveSizes(ctx, repo, nodes, blobOIDs, blobIndex); err != nil {
		// Non-fatal: sizes remain "unknown" and reads will still work via
		// hydration. Log so operators can diagnose size=0 issues.
		s.logger.Warn("batch size resolution failed, files will show size 0 until hydrated", "repo", repo.Name, "error", err)
	}
	return addImplicitDirs(repo.ID, nodes), nil
}

func (s *Store) batchResolveSizes(ctx context.Context, repo model.RepoConfig, nodes []model.BaseNode, oids []string, index map[string][]int) error {
	if len(oids) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch-check", "--buffer")
	cmd.Env = append(os.Environ(), "GIT_DIR="+repo.GitDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		return err
	}
	for _, oid := range oids {
		fmt.Fprintln(stdin, oid)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		return err
	}
	// Output format: "<oid> <type> <size>" or "<oid> missing"
	scan := bufio.NewScanner(&outBuf)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 3 {
			continue
		}
		oid := fields[0]
		sizeStr := fields[2]
		sz, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil {
			continue
		}
		for _, idx := range index[oid] {
			nodes[idx].SizeBytes = sz
			nodes[idx].SizeState = "known"
		}
	}
	return scan.Err()
}

// BlobToCache fetches a git object and writes it to dstPath in a binary-safe manner.
// It pipes stdout directly to a temp file to avoid string conversion that would
// corrupt binary content.
func (s *Store) BlobToCache(ctx context.Context, repo model.RepoConfig, objectOID string, dstPath string) (size int64, err error) {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, err
	}
	tmp := dstPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}

	cmd := exec.CommandContext(ctx, "git", "cat-file", "-p", objectOID)
	cmd.Env = append(os.Environ(), "GIT_DIR="+repo.GitDir)
	cmd.Stdout = f
	errBuf := &bytes.Buffer{}
	cmd.Stderr = errBuf

	runErr := cmd.Run()
	_ = f.Sync()
	closeErr := f.Close()

	if runErr != nil {
		os.Remove(tmp)
		msg := auth.RedactString(strings.TrimSpace(errBuf.String()))
		if msg == "" {
			msg = auth.RedactString(runErr.Error())
		}
		return 0, errors.New(msg)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return 0, closeErr
	}

	st, err := os.Stat(tmp)
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return st.Size(), nil
}

func (s *Store) ComputeAheadBehind(ctx context.Context, repo model.RepoConfig) (ahead int, behind int, diverged bool, err error) {
	rangeSpec := fmt.Sprintf("HEAD...origin/%s", repo.Branch)
	out, err := runGit(ctx, repo.GitDir, "rev-list", "--left-right", "--count", rangeSpec)
	if err != nil {
		if strings.Contains(err.Error(), "unknown revision") {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	parts := strings.Fields(out)
	if len(parts) < 2 {
		return 0, 0, false, nil
	}
	ahead, _ = strconv.Atoi(parts[0])
	behind, _ = strconv.Atoi(parts[1])
	diverged = ahead > 0 && behind > 0
	return ahead, behind, diverged, nil
}

func runGit(ctx context.Context, gitDir string, args ...string) (string, error) {
	return runGitWithEnv(ctx, gitDir, nil, args...)
}

func runGitWithEnv(ctx context.Context, gitDir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	env := os.Environ()
	if gitDir != "" {
		env = append(env, "GIT_DIR="+gitDir)
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	buf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.Stdout = buf
	cmd.Stderr = errBuf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err == nil {
		return out, nil
	}
	msg := auth.RedactString(strings.TrimSpace(errBuf.String()))
	if msg == "" {
		msg = auth.RedactString(err.Error())
	}
	return out, errors.New(msg)
}

// credentialEnv returns a sanitized URL (safe for ps) and env vars that
// configure a one-shot git credential helper to supply the real credentials.
func credentialEnv(rawURL string) (safeURL string, env []string) {
	if rawURL == "" {
		return "", nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL, nil
	}
	username := u.User.Username()
	password, hasPassword := u.User.Password()
	if username == "" && !hasPassword {
		return rawURL, nil
	}

	// Build a credential helper that prints credentials to stdout.
	// Uses printf to avoid shell quoting issues with single quotes in passwords.
	var lines []string
	if hasPassword {
		lines = append(lines, "username="+username, "password="+password)
	} else if username != "" {
		// Token-as-username pattern (e.g., https://ghp_xxx@github.com)
		lines = append(lines, "username="+username, "password="+username)
	}
	// Escape single quotes in the credential payload to prevent shell injection.
	payload := strings.Join(lines, "\n")
	payload = strings.ReplaceAll(payload, "'", "'\\''")
	helper := fmt.Sprintf("!f() { printf '%%s\\n' '%s'; }; f", payload)

	u.User = nil
	return u.String(), []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=" + helper,
	}
}

func rootNode(repoID model.RepoID) model.BaseNode {
	return model.BaseNode{
		RepoID:    repoID,
		Path:      ".",
		Type:      "dir",
		Mode:      0o755,
		ObjectOID: "",
		SizeState: "known",
	}
}

func normalizeGitType(t string, mode uint32) string {
	// Symlinks are reported as type "blob" with mode 120000
	if mode&0o170000 == 0o120000 {
		return "symlink"
	}
	switch t {
	case "blob":
		return "file"
	case "tree":
		return "dir"
	default:
		return "file"
	}
}

func addImplicitDirs(repoID model.RepoID, nodes []model.BaseNode) []model.BaseNode {
	seen := map[string]bool{".": true}
	for _, n := range nodes {
		seen[n.Path] = true
	}
	for _, n := range nodes {
		d := filepath.Dir(n.Path)
		for d != "." && d != "/" && !seen[d] {
			seen[d] = true
			nodes = append(nodes, model.BaseNode{
				RepoID:    repoID,
				Path:      d,
				Type:      "dir",
				Mode:      0o755,
				SizeState: "known",
			})
			d = filepath.Dir(d)
		}
	}
	return nodes
}
