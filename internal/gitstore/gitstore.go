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

type readBlobResult struct {
	data []byte
	err  error
}

const maxReadBlobBytes int64 = 1<<31 - 1

const fetchedFullRefRemoteTrackingRef = "refs/remotes/artifact-fs/fetch-ref"

type fetchRefInfo struct {
	sourceRef string
	remoteRef string
	branch    string
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
	safeURL, credHelper, err := credentialEnv(cfg.RemoteURL)
	if err != nil {
		return err
	}
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
	target, err := fetchRefTarget(repo, ref)
	if err != nil {
		return err
	}
	refspec := "+" + target.sourceRef + ":" + target.remoteRef
	if target.branch != "" {
		refspec = fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", target.branch, target.branch)
	}
	_, err = runGitWithEnv(ctx, repo.GitDir, nonInteractiveGitEnv(), "fetch", "--filter=blob:none", "origin", refspec)
	return err
}

func (s *Store) PrepareExistingCloneNonInteractive(ctx context.Context, repo model.RepoConfig) error {
	if strings.TrimSpace(repo.RemoteURL) == "" {
		return errors.New("remote URL is required")
	}
	safeURL, _, err := credentialEnv(repo.RemoteURL)
	if err != nil {
		return err
	}
	if safeURL != repo.RemoteURL {
		return errors.New("existing clone remote must use ambient credentials")
	}
	remoteURL, err := runGit(ctx, repo.GitDir, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if strings.TrimSpace(remoteURL) != strings.TrimSpace(repo.RemoteURL) {
		if _, err := runGitWithEnv(ctx, repo.GitDir, nonInteractiveGitEnv(), "remote", "set-url", "origin", repo.RemoteURL); err != nil {
			return err
		}
	}
	if err := s.FetchRefNonInteractive(ctx, repo, repo.FetchRef); err != nil {
		return err
	}
	return s.PrepareFetchedBranch(ctx, repo, repo.FetchRef)
}

func (s *Store) PrepareFetchedBranch(ctx context.Context, repo model.RepoConfig, ref string) error {
	target, err := fetchRefTarget(repo, ref)
	if err != nil {
		return err
	}
	oid, err := runGit(ctx, repo.GitDir, "rev-parse", "--verify", target.remoteRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("remote ref %s missing after fetch: %w", target.remoteRef, err)
	}
	oid = strings.TrimSpace(oid)
	if target.branch == "" {
		if _, err := runGit(ctx, repo.GitDir, "update-ref", "--no-deref", "HEAD", oid); err != nil {
			return err
		}
		return s.ReadTreeHEAD(ctx, repo)
	}
	if _, err := runGit(ctx, repo.GitDir, "update-ref", "refs/heads/"+target.branch, oid); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, "symbolic-ref", "HEAD", "refs/heads/"+target.branch); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, "branch", "--set-upstream-to", "origin/"+target.branch, target.branch); err != nil {
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
	remoteURL, err := runGit(ctx, repo.GitDir, "remote", "get-url", "origin")
	if err == nil && remoteHasInlineCredentials(remoteURL) {
		return errors.New("prepared git dir origin must use ambient credentials")
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

func (s *Store) ReadBlob(ctx context.Context, repo model.RepoConfig, objectOID string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("negative max bytes: %d", maxBytes)
	}
	pool := s.getPool(repo.GitDir)
	batch, err := pool.acquire()
	if err != nil {
		return nil, err
	}
	data, err := readBatchBlob(ctx, batch, objectOID, maxBytes)
	if err == nil {
		pool.release(batch)
		return data, nil
	}
	if errors.Is(err, model.ErrBlobTooLarge) {
		batch.kill()
		return nil, err
	}
	batch.close()

	batch, err = pool.acquire()
	if err != nil {
		return nil, err
	}
	data, err = readBatchBlob(ctx, batch, objectOID, maxBytes)
	if err != nil {
		if errors.Is(err, model.ErrBlobTooLarge) {
			batch.kill()
			return nil, err
		}
		batch.close()
		return nil, err
	}
	pool.release(batch)
	return data, nil
}

func readBatchBlob(ctx context.Context, batch *batchCatFile, objectOID string, maxBytes int64) ([]byte, error) {
	ch := make(chan readBlobResult, 1)
	go func() {
		data, err := batch.readBlob(objectOID, maxBytes)
		ch <- readBlobResult{data: data, err: err}
	}()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-ctx.Done():
		batch.kill()
		return nil, ctx.Err()
	}
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

func (b *batchCatFile) kill() {
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
	b.close()
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

	size, err := b.readObjectSize(oid)
	if err != nil {
		return 0, err
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

func (b *batchCatFile) readBlob(oid string, maxBytes int64) ([]byte, error) {
	if b.cmd == nil || b.stdin == nil {
		return nil, errors.New("batch cat-file process not running")
	}
	if _, err := fmt.Fprintf(b.stdin, "%s\n", oid); err != nil {
		return nil, fmt.Errorf("batch write: %w", err)
	}
	size, err := b.readObjectSize(oid)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, fmt.Errorf("negative blob size: %d", size)
	}
	if size > maxBytes {
		return nil, model.ErrBlobTooLarge
	}
	if size > maxReadBlobBytes {
		return nil, model.ErrBlobTooLarge
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(b.stdout, data); err != nil {
		return nil, fmt.Errorf("batch read content: %w", err)
	}
	if _, err := b.stdout.ReadByte(); err != nil {
		return nil, fmt.Errorf("batch read trailing LF: %w", err)
	}
	return data, nil
}

func (b *batchCatFile) readObjectSize(oid string) (int64, error) {
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
func credentialEnv(rawURL string) (safeURL string, env []string, err error) {
	if rawURL == "" {
		return "", nil, nil
	}
	if strings.ContainsAny(rawURL, "?#") {
		return "", nil, errors.New("remote URL must not include query or fragment")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		if remoteHasInlineCredentials(rawURL) {
			return "", nil, errors.New("malformed remote URL")
		}
		if rawUserinfoCandidateHasPassword(rawURL) {
			return "", nil, errors.New("malformed remote URL")
		}
		if strings.Contains(rawURL, "://") {
			return "", nil, errors.New("malformed remote URL")
		}
		return rawURL, nil, nil
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(rawURL, "#") {
		return "", nil, errors.New("remote URL must not include query or fragment")
	}
	if u.User == nil && strings.Contains(rawURL, "@") && (auth.HasInlineCredentials(rawURL) || malformedUserinfoInRemote(rawURL, u)) {
		return "", nil, errors.New("malformed remote URL")
	}
	if u.User == nil {
		return rawURL, nil, nil
	}
	if !isHTTPRemote(rawURL, u.Scheme) {
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", nil, errors.New("remote URL includes unsupported inline credentials")
		}
		return rawURL, nil, nil
	}
	username := u.User.Username()
	password, hasPassword := u.User.Password()
	if username == "" && !hasPassword {
		return rawURL, nil, nil
	}

	credentialUsername := username
	credentialPassword := password
	if hasPassword {
		credentialPassword = password
	} else if username != "" {
		// Token-as-username pattern (e.g., https://ghp_xxx@github.com)
		credentialPassword = username
	}
	helper := "!f() { printf '%s\\n' \"username=$ARTIFACT_FS_GIT_USERNAME\" \"password=$ARTIFACT_FS_GIT_PASSWORD\"; }; f"

	u.User = nil
	return u.String(), []string{
		"GIT_TERMINAL_PROMPT=0",
		"ARTIFACT_FS_GIT_USERNAME=" + credentialUsername,
		"ARTIFACT_FS_GIT_PASSWORD=" + credentialPassword,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=" + helper,
	}, nil
}

func isHTTPRemote(rawURL string, scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https":
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(rawURL))
	return strings.HasPrefix(lower, "http:/") || strings.HasPrefix(lower, "https:/") ||
		strings.HasPrefix(lower, "http//") || strings.HasPrefix(lower, "https//") ||
		strings.HasPrefix(lower, "http:") || strings.HasPrefix(lower, "https:")
}

func isMalformedHTTPUserinfo(rawURL string, u *url.URL) bool {
	if !isHTTPRemote(rawURL, u.Scheme) {
		return false
	}
	if u.Host == "" {
		return true
	}
	return strings.HasPrefix(u.Path, "/@")
}

func remoteHasInlineCredentials(rawURL string) bool {
	if strings.ContainsAny(rawURL, "?#") {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return auth.HasInlineCredentials(rawURL) || rawUserinfoCandidateHasPassword(rawURL)
	}
	if u.User != nil {
		_, hasPassword := u.User.Password()
		return isHTTPRemote(rawURL, u.Scheme) || hasPassword
	}
	return strings.Contains(rawURL, "@") && malformedUserinfoInRemote(rawURL, u)
}

func malformedUserinfoInRemote(rawURL string, u *url.URL) bool {
	if isHTTPRemote(rawURL, u.Scheme) {
		return auth.HasInlineCredentials(rawURL)
	}
	if isMalformedHTTPUserinfo(rawURL, u) {
		return true
	}
	if isHTTPRemote(rawURL, u.Scheme) && strings.Contains(u.Hostname(), ".") {
		return false
	}
	return rawUserinfoCandidateHasPassword(rawURL)
}

func rawUserinfoCandidateHasPassword(raw string) bool {
	prefix := raw
	start := -1
	if i := strings.LastIndex(prefix, "://"); i >= 0 {
		start = i + len("://")
	} else if i := strings.Index(prefix, ":/"); i >= 0 {
		start = i + len(":/")
	} else if i := strings.Index(prefix, "//"); i >= 0 {
		start = i + len("//")
	} else if i := strings.Index(prefix, ":"); i >= 0 {
		start = i + len(":")
	}
	if start < 0 || start >= len(raw) {
		return false
	}
	endChars := "?#"
	if strings.Contains(raw, "://") {
		endChars = "/?#"
	}
	end := len(raw)
	if relEnd := strings.IndexAny(raw[start:], endChars); relEnd >= 0 {
		end = start + relEnd
	}
	at := strings.LastIndex(raw[start:end], "@")
	if at < 0 {
		return false
	}
	at += start
	return strings.Contains(raw[start:at], ":")
}

func nonInteractiveGitEnv() []string {
	return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + sshBatchModeCommand(os.Getenv("GIT_SSH_COMMAND"))}
}

func sshBatchModeCommand(existing string) string {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		return "ssh -o BatchMode=yes"
	}
	tokens := splitShellFields(existing)
	filtered := make([]string, 0, len(tokens)+2)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		lower := strings.ToLower(tok)
		if lower == "-o" && i+1 < len(tokens) && isBatchModeOption(tokens[i+1]) {
			i++
			continue
		}
		if strings.HasPrefix(lower, "-obatchmode=") {
			continue
		}
		filtered = append(filtered, tok)
	}
	if len(filtered) == 0 {
		filtered = append(filtered, "ssh")
	}
	filtered = append(filtered, "-o", "BatchMode=yes")
	for i, tok := range filtered {
		filtered[i] = shellQuote(tok)
	}
	return strings.Join(filtered, " ")
}

func splitShellFields(s string) []string {
	var fields []string
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range s {
		if escaped {
			if r == '$' {
				b.WriteString(`\$`)
			} else {
				b.WriteRune(r)
			}
			escaped = false
			continue
		}
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '$' && quote == '\'' {
				b.WriteString(`\$`)
			} else if r == '\\' && quote == '"' {
				escaped = true
			} else {
				b.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '\\':
			escaped = true
		case r == ' ' || r == '\t' || r == '\n':
			if b.Len() > 0 {
				fields = append(fields, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if b.Len() > 0 {
		fields = append(fields, b.String())
	}
	return fields
}

func isBatchModeOption(opt string) bool {
	parts := strings.Fields(strings.ToLower(strings.TrimSpace(opt)))
	if len(parts) == 0 {
		return false
	}
	return parts[0] == "batchmode" || strings.HasPrefix(parts[0], "batchmode=")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.Contains(s, "$") {
		return doubleQuote(s)
	}
	if isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func doubleQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '$' {
			b.WriteString(`\$`)
			i++
			continue
		}
		switch s[i] {
		case '\\', '"', '`':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func isShellSafe(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if strings.ContainsRune("@%_+=:,./-~$", r) {
			continue
		}
		return false
	}
	return true
}

func fetchRefTarget(repo model.RepoConfig, ref string) (fetchRefInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = strings.TrimSpace(repo.Branch)
	}
	if ref == "" {
		return fetchRefInfo{}, errors.New("fetch ref is required")
	}
	if branch := branchName(ref); branch != "" {
		return fetchRefInfo{
			sourceRef: "refs/heads/" + branch,
			remoteRef: "refs/remotes/origin/" + branch,
			branch:    branch,
		}, nil
	}
	if strings.HasPrefix(ref, "refs/") {
		return fetchRefInfo{sourceRef: ref, remoteRef: fetchedFullRefRemoteTrackingRef}, nil
	}
	return fetchRefInfo{}, errors.New("fetch ref is required")
}

func branchName(ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "refs/heads/") {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	if strings.HasPrefix(ref, "refs/remotes/origin/") {
		return strings.TrimPrefix(ref, "refs/remotes/origin/")
	}
	if strings.HasPrefix(ref, "origin/") {
		return strings.TrimPrefix(ref, "origin/")
	}
	if strings.HasPrefix(ref, "refs/") {
		return ""
	}
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
