package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type Store struct {
	logger      *slog.Logger
	mu          sync.Mutex
	poolMaxSize int
	pools       map[string]*batchPool // gitDir -> pool
}

func New(logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{logger: logger, poolMaxSize: 4, pools: map[string]*batchPool{}}
}

// Close shuts down all persistent batch processes.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for dir, p := range s.pools {
		p.closeAll()
		delete(s.pools, dir)
	}
}

func (s *Store) SetBatchPoolSize(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.poolMaxSize = n
	for _, p := range s.pools {
		p.setMaxSize(n)
	}
}

func (s *Store) CloneBlobless(ctx context.Context, cfg model.RepoConfig) error {
	return s.cloneBlobless(ctx, cfg, nil)
}

func (s *Store) CloneBloblessNonInteractive(ctx context.Context, cfg model.RepoConfig) error {
	return s.cloneBlobless(ctx, cfg, nonInteractiveGitEnv())
}

func (s *Store) cloneBlobless(ctx context.Context, cfg model.RepoConfig, extraEnv []string) error {
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
	env := append([]string{}, extraEnv...)
	env = append(env, credHelper...)

	args := []string{"clone", "--filter=blob:none", "--no-checkout", "--single-branch", "--branch", cfg.Branch, safeURL, target}
	if _, err := runGitWithEnv(ctx, "", env, args...); err != nil {
		return err
	}
	// Populate the index so git status works inside the mount.
	if _, err := runGit(ctx, filepath.Join(target, ".git"), "read-tree", "HEAD"); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(target, ".git"), cfg.GitDir); err != nil {
		return err
	}
	return nil
}

func (s *Store) Fetch(ctx context.Context, repo model.RepoConfig) error {
	_, err := runGit(ctx, repo.GitDir, "fetch", "origin")
	return err
}

func (s *Store) FetchRefNonInteractive(ctx context.Context, repo model.RepoConfig, ref string) error {
	branch := branchName(ref)
	if branch == "" {
		branch = branchName(repo.Branch)
	}
	if branch == "" {
		return errors.New("fetch ref is required")
	}
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch)
	_, err := runGitWithEnv(ctx, repo.GitDir, nonInteractiveGitEnv(), "fetch", "--filter=blob:none", "origin", refspec)
	return err
}

func (s *Store) PrepareFetchedBranch(ctx context.Context, repo model.RepoConfig, ref string) error {
	branch := branchName(ref)
	if branch == "" {
		branch = branchName(repo.Branch)
	}
	if branch == "" {
		return errors.New("fetch ref is required")
	}
	remoteRef := "refs/remotes/origin/" + branch
	oid, err := runGit(ctx, repo.GitDir, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("remote ref %s missing after fetch: %w", remoteRef, err)
	}
	if _, err := runGit(ctx, repo.GitDir, "update-ref", "refs/heads/"+branch, strings.TrimSpace(oid)); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, "branch", "--set-upstream-to", "origin/"+branch, branch); err != nil {
		s.logger.Warn("set upstream failed", "repo", repo.Name, "error", err)
	}
	return s.ReadTreeHEAD(ctx, repo)
}

func (s *Store) ValidatePreparedGitDir(ctx context.Context, repo model.RepoConfig) error {
	if strings.TrimSpace(repo.GitDir) == "" {
		return errors.New("git dir is required")
	}
	st, err := os.Stat(repo.GitDir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("git dir %s is not a directory", repo.GitDir)
	}
	if _, err := runGit(ctx, repo.GitDir, "rev-parse", "--git-dir"); err != nil {
		return err
	}
	return nil
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
	// -z: NUL-delimited output with raw paths (no C-quoting of non-ASCII names).
	out, err := runGit(ctx, repo.GitDir, "ls-tree", "-r", "-t", "-z", headOID)
	if err != nil {
		return nil, err
	}
	records := strings.Split(out, "\x00")
	nodes := []model.BaseNode{rootNode(repo.ID)}
	var blobOIDs []string
	blobIndex := map[string][]int{} // oid -> indices into nodes
	for _, line := range records {
		if line == "" {
			continue
		}
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
	// GIT_NO_LAZY_FETCH prevents batch-check from fetching blob metadata from
	// the promisor remote on blobless clones. Without it, every blob OID
	// triggers a network round-trip, turning a millisecond operation into
	// minutes. Blobs reported as "missing" keep SizeState="unknown" and get
	// their size resolved during hydration.
	cmd.Env = append(os.Environ(), "GIT_DIR="+repo.GitDir, "GIT_NO_LAZY_FETCH=1")
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
// Uses a persistent cat-file --batch process to amortize process spawn and
// remote connection costs across multiple blob fetches.
func (s *Store) BlobToCache(ctx context.Context, repo model.RepoConfig, objectOID string, dstPath string) (size int64, err error) {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, err
	}
	pool := s.getPool(repo.GitDir)
	batch, err := pool.acquire()
	if err != nil {
		return 0, err
	}
	size, err = batch.fetchToFile(objectOID, dstPath)
	if err != nil {
		// Process may have died or be desynchronized; discard and retry.
		batch.close()
		batch, err = pool.acquire()
		if err != nil {
			return 0, err
		}
		size, err = batch.fetchToFile(objectOID, dstPath)
		if err != nil {
			// Retry also failed; close instead of returning a potentially
			// corrupted process to the pool.
			batch.close()
			return 0, err
		}
	}
	pool.release(batch)
	return size, err
}

func (s *Store) VerifyBlob(ctx context.Context, repo model.RepoConfig, objectOID string, cachePath string) (bool, error) {
	out, err := runGit(ctx, repo.GitDir, "hash-object", "--no-filters", cachePath)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == objectOID, nil
}

func (s *Store) getPool(gitDir string) *batchPool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pools[gitDir]; ok {
		return p
	}
	p := &batchPool{gitDir: gitDir, logger: s.logger, maxSize: s.poolMaxSize}
	s.pools[gitDir] = p
	return p
}

// batchPool maintains a pool of reusable cat-file --batch processes so
// multiple hydrator workers can fetch blobs concurrently.
type batchPool struct {
	mu      sync.Mutex
	free    []*batchCatFile
	gitDir  string
	logger  *slog.Logger
	maxSize int
}

func (p *batchPool) acquire() (*batchCatFile, error) {
	p.mu.Lock()
	if n := len(p.free); n > 0 {
		b := p.free[n-1]
		p.free = p.free[:n-1]
		p.mu.Unlock()
		if b.alive() {
			return b, nil
		}
		b.close()
	} else {
		p.mu.Unlock()
	}
	return newBatchCatFile(p.gitDir, p.logger)
}

func (p *batchPool) release(b *batchCatFile) {
	if !b.alive() {
		b.close()
		return
	}
	p.mu.Lock()
	if len(p.free) < p.maxSize {
		p.free = append(p.free, b)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	b.close()
}

func (p *batchPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.free {
		b.close()
	}
	p.free = nil
}

func (p *batchPool) setMaxSize(n int) {
	var extras []*batchCatFile
	p.mu.Lock()
	p.maxSize = n
	if len(p.free) > n {
		extras = append(extras, p.free[n:]...)
		p.free = p.free[:n]
	}
	p.mu.Unlock()
	for _, b := range extras {
		b.close()
	}
}

// batchCatFile manages a persistent `git cat-file --batch` process. The
// persistent process amortizes process startup and (on blobless clones)
// remote connection costs across multiple blob fetches. Callers must ensure
// exclusive access (the batchPool handles this).
type batchCatFile struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	logger *slog.Logger
}

func newBatchCatFile(gitDir string, logger *slog.Logger) (*batchCatFile, error) {
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("batch cat-file stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("batch cat-file stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("batch cat-file start: %w", err)
	}
	return &batchCatFile{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 256*1024),
		logger: logger,
	}, nil
}

func (b *batchCatFile) alive() bool {
	return b.cmd != nil && b.cmd.Process != nil && b.cmd.ProcessState == nil
}

func (b *batchCatFile) close() {
	if b.stdin != nil {
		b.stdin.Close()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		b.cmd.Wait()
	}
}

// fetchToFile writes oid to the batch process stdin, reads the response header
// and streams the blob content directly to dstPath. Binary-safe (no string
// conversion of blob content).
func (b *batchCatFile) fetchToFile(oid string, dstPath string) (int64, error) {
	if b.cmd == nil || b.stdin == nil {
		return 0, errors.New("batch cat-file process not running")
	}

	// Request the object
	if _, err := fmt.Fprintf(b.stdin, "%s\n", oid); err != nil {
		return 0, fmt.Errorf("batch write: %w", err)
	}

	// Read response header: "<oid> SP <type> SP <size> LF" or "<oid> SP missing LF"
	header, err := b.stdout.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("batch read header: %w", err)
	}
	header = strings.TrimRight(header, "\n")
	fields := strings.Fields(header)
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected batch header: %q", header)
	}
	if fields[1] == "missing" {
		return 0, fmt.Errorf("object %s missing", oid)
	}
	if len(fields) < 3 {
		return 0, fmt.Errorf("unexpected batch header: %q", header)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", fields[2], err)
	}

	// Stream blob content to a temp file, then atomic rename. The blob cache is
	// reconstructible from git, so we prefer throughput over per-object fsync.
	tmp := dstPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		// Drain the blob content so the protocol stays in sync.
		io.CopyN(io.Discard, b.stdout, size+1) // +1 for trailing LF
		return 0, err
	}
	written, copyErr := io.CopyN(f, b.stdout, size)
	// Read the trailing LF that git appends after the content. If this fails
	// the batch protocol is desynchronized and the caller must discard the
	// process.
	if _, lfErr := b.stdout.ReadByte(); lfErr != nil && copyErr == nil {
		copyErr = fmt.Errorf("batch read trailing LF: %w", lfErr)
	}
	closeErr := f.Close()

	if copyErr != nil || written != size {
		os.Remove(tmp)
		if copyErr != nil {
			return 0, fmt.Errorf("batch read content: %w", copyErr)
		}
		return 0, fmt.Errorf("short read: got %d, want %d", written, size)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("close temp blob file: %w", closeErr)
	}

	if err := os.Rename(tmp, dstPath); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return size, nil
}

// CommitTimestamp returns the committer timestamp of the given commit OID.
func (s *Store) CommitTimestamp(ctx context.Context, repo model.RepoConfig, oid string) (int64, error) {
	out, err := runGit(ctx, repo.GitDir, "show", "-s", "--format=%ct", oid)
	if err != nil {
		return 0, err
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse commit timestamp %q: %w", out, err)
	}
	return ts, nil
}

// ReadTreeHEAD updates the git index to match HEAD. Must be called after HEAD
// changes (branch switch, commit) so git status inside the mount is correct.
func (s *Store) ReadTreeHEAD(ctx context.Context, repo model.RepoConfig) error {
	_, err := runGit(ctx, repo.GitDir, "read-tree", "HEAD")
	return err
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

func nonInteractiveGitEnv() []string {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

func branchName(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/remotes/origin/")
	ref = strings.TrimPrefix(ref, "origin/")
	return ref
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
