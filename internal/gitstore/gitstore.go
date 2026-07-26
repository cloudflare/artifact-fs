package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type Store struct {
	logger         *slog.Logger
	mu             sync.Mutex
	poolMaxSize    int
	pools          map[string]*batchPool // gitDir -> pool
	gitRetryDelays []time.Duration
	closed         bool
}

// GitOperation identifies an explicitly retried Git operation.
type GitOperation string

const (
	GitOperationClone GitOperation = "clone"
	GitOperationFetch GitOperation = "fetch"

	logGitOperationAttemptFailed = "git operation attempt failed"
	logGitOperationRetrying      = "retrying transient git operation failure"
	logGitOperationRecovered     = "git operation recovered"
	logGitOperationInterrupted   = "git operation interrupted during retry backoff"
)

// GitOperationError describes a failed Git operation without exposing
// credentials. Retryable reports whether its command failure matched a known
// transient smart-Git transport symptom.
type GitOperationError struct {
	Operation GitOperation
	Attempts  int
	Retryable bool

	cause      error
	contextErr error
}

func (e *GitOperationError) Error() string {
	if e == nil {
		return "git operation failed"
	}
	operation := string(e.Operation)
	if operation == "" {
		operation = "operation"
	}
	var msg string
	if e.Attempts == 0 {
		msg = fmt.Sprintf("git %s failed before first attempt", operation)
	} else {
		noun := "attempts"
		if e.Attempts == 1 {
			noun = "attempt"
		}
		msg = fmt.Sprintf("git %s failed after %d %s", operation, e.Attempts, noun)
	}
	if e.cause != nil {
		msg += ": " + auth.RedactString(e.cause.Error())
	}
	if e.contextErr != nil {
		msg += "; caller context: " + e.contextErr.Error()
	}
	return msg
}

func (e *GitOperationError) Unwrap() []error {
	if e == nil {
		return nil
	}
	errs := make([]error, 0, 2)
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	if e.contextErr != nil {
		errs = append(errs, e.contextErr)
	}
	return errs
}

// gitCommandError preserves the command failure and caller cancellation as
// distinct process-boundary errors. cause has already been redacted.
type gitCommandError struct {
	cause      error
	contextErr error
	retryable  bool // Classified before cause is redacted.
}

type redactedCause struct {
	message string
	cause   error
}

func (e *redactedCause) Error() string { return e.message }
func (e *redactedCause) Unwrap() error { return e.cause }

func (e *gitCommandError) Error() string {
	if e == nil {
		return "git command failed"
	}
	if e.cause == nil {
		if e.contextErr != nil {
			return "caller context: " + e.contextErr.Error()
		}
		return "git command failed"
	}
	msg := e.cause.Error()
	if e.contextErr != nil {
		msg += "; caller context: " + e.contextErr.Error()
	}
	return msg
}

func (e *gitCommandError) Unwrap() []error {
	if e == nil {
		return nil
	}
	errs := make([]error, 0, 2)
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	if e.contextErr != nil {
		errs = append(errs, e.contextErr)
	}
	return errs
}

var defaultGitRetryDelays = []time.Duration{250 * time.Millisecond, time.Second}

type readBlobResult struct {
	data []byte
	err  error
}

type fetchBlobResult struct {
	size int64
	err  error
}

const maxReadBlobBytes int64 = 1<<31 - 1

const (
	maxGitErrorBytes       = 16 << 10
	gitErrorScanOverlap    = 128
	gitErrorTruncatedLabel = "[git stderr truncated]"
)

type boundedGitError struct {
	buf          bytes.Buffer
	limit        int
	truncated    bool
	scanTail     string
	sawPermanent bool
	sawTransient bool
}

func (b *boundedGitError) Write(p []byte) (int, error) {
	n := len(p)
	scan := strings.ToLower(b.scanTail + string(p))
	b.sawPermanent = b.sawPermanent || containsGitSymptom(scan, permanentGitFailureSymptoms)
	b.sawTransient = b.sawTransient || containsGitSymptom(scan, transientGitFailureSymptoms)
	if len(scan) > gitErrorScanOverlap {
		b.scanTail = scan[len(scan)-gitErrorScanOverlap:]
	} else {
		b.scanTail = scan
	}
	remaining := max(b.limit-b.buf.Len(), 0)
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	if len(p) > 0 {
		_, _ = b.buf.Write(p)
	}
	return n, nil
}

func (b *boundedGitError) String() string {
	if !b.truncated {
		return b.buf.String()
	}
	return strings.TrimRight(b.buf.String(), "\r\n") + "\n" + gitErrorTruncatedLabel
}

func (b *boundedGitError) Retryable() bool {
	return !b.sawPermanent && b.sawTransient
}

func redactTruncatedCredentialPrefix(message string, credentials []string) string {
	suffix := "\n" + gitErrorTruncatedLabel
	body, truncated := strings.CutSuffix(message, suffix)
	if !truncated {
		return message
	}
	for _, credential := range credentials {
		for prefixLen := len(credential) - 1; prefixLen > 0; prefixLen-- {
			if strings.HasSuffix(body, credential[:prefixLen]) {
				body = body[:len(body)-prefixLen] + "REDACTED"
				break
			}
		}
	}
	return body + suffix
}

const fetchedFullRefRemoteTrackingRef = "refs/remotes/artifact-fs/fetch-ref"
const verifiedSourceTrackingRef = "refs/remotes/artifact-fs/source"

type fetchRefInfo struct {
	sourceRef string
	remoteRef string
	branch    string
}

func New(logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{
		logger:         logger,
		poolMaxSize:    4,
		pools:          map[string]*batchPool{},
		gitRetryDelays: append([]time.Duration(nil), defaultGitRetryDelays...),
	}
}

// Close shuts down all persistent batch processes.
func (s *Store) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	pools := make([]*batchPool, 0, len(s.pools))
	for dir, p := range s.pools {
		pools = append(pools, p)
		delete(s.pools, dir)
	}
	s.mu.Unlock()
	for _, p := range pools {
		p.closeAll()
	}
}

func (s *Store) SetBatchPoolSize(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
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
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(cfg.GitDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	// Strip credentials from the CLI-visible URL; pass them via a credential helper
	// so they don't appear in ps output.
	safeURL, credHelper, err := credentialEnv(cfg.RemoteURL)
	if err != nil {
		return err
	}
	env := append([]string{}, extraEnv...)
	env = append(env, credHelper...)

	var target string
	attempt := func() error {
		var err error
		// A failed git clone can leave files in its destination, so every retry
		// gets a fresh directory rather than risking a false "already exists".
		target, err = os.MkdirTemp(parent, ".clone-*")
		if err != nil {
			return fmt.Errorf("mktemp clone dir: %w", err)
		}
		sourceRef := strings.TrimSpace(cfg.Branch)
		branch := sourceRef
		if after, ok := strings.CutPrefix(sourceRef, "refs/heads/"); ok {
			branch = after
		} else if strings.HasPrefix(sourceRef, "refs/") {
			branch = ""
		}
		if branch != "" {
			args := []string{"clone", "--filter=blob:none"}
			if cfg.HistoryDepth > 0 {
				args = append(args, fmt.Sprintf("--depth=%d", cfg.HistoryDepth))
			}
			args = append(args, "--no-checkout", "--single-branch", "--no-tags", "--branch", branch, safeURL, target)
			_, err = runGitWithEnv(ctx, "", env, args...)
		} else {
			err = cloneExactRef(ctx, target, safeURL, env, sourceRef, cfg.HistoryDepth)
		}
		if err != nil {
			_ = os.RemoveAll(target)
			target = ""
		}
		return err
	}
	if err := s.retryGitOperationForRemotes(ctx, GitOperationClone, cfg.Name, []string{cfg.RemoteURL}, attempt); err != nil {
		return err
	}
	defer os.RemoveAll(target)

	targetGitDir := filepath.Join(target, ".git")

	// Populate the index so git status works inside the mount.
	if _, err := runGit(ctx, targetGitDir, "read-tree", "HEAD"); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(target, ".git"), cfg.GitDir); err != nil {
		return err
	}
	s.invalidateBatchPool(cfg.GitDir)
	return nil
}

// PrepareSource acquires and verifies one exact remote source revision.
func (s *Store) PrepareSource(ctx context.Context, cfg model.RepoConfig, requirement model.SourceRequirement) (model.PreparedSource, error) {
	requirement.Ref = strings.TrimSpace(requirement.Ref)
	requirement.RequiredCommit = strings.ToLower(strings.TrimSpace(requirement.RequiredCommit))
	if requirement.Ref == "" || !strings.HasPrefix(requirement.Ref, "refs/") {
		return model.PreparedSource{}, errors.New("source ref must be a canonical refs/... name")
	}
	if _, err := runGit(ctx, "", "check-ref-format", requirement.Ref); err != nil {
		return model.PreparedSource{}, fmt.Errorf("invalid source ref %q", requirement.Ref)
	}
	if requirement.RequiredCommit == "" {
		return model.PreparedSource{}, errors.New("required commit is required")
	}
	if requirement.Depth < 0 {
		return model.PreparedSource{}, errors.New("history depth must not be negative")
	}

	prepared := model.PreparedSource{Ref: requirement.Ref, Commit: requirement.RequiredCommit, Verified: true}
	if cfg.AcquiredRef == requirement.Ref && strings.EqualFold(cfg.AcquiredCommit, requirement.RequiredCommit) {
		available, err := requiredCommitAvailable(ctx, cfg.GitDir, requirement.RequiredCommit)
		if err != nil {
			return model.PreparedSource{}, err
		}
		if available {
			if err := prepareFixedHEAD(ctx, cfg.GitDir, requirement.RequiredCommit, false); err != nil {
				return model.PreparedSource{}, err
			}
			return prepared, nil
		}
	}

	parent := filepath.Dir(cfg.GitDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return model.PreparedSource{}, err
	}
	safeURL, credHelper, err := credentialEnv(cfg.RemoteURL)
	if err != nil {
		return model.PreparedSource{}, err
	}
	env := append(nonInteractiveGitEnv(), credHelper...)
	var candidateGitDir string
	attempt := func() error {
		if candidateGitDir != "" {
			_ = os.RemoveAll(candidateGitDir)
		}
		candidateGitDir, err = os.MkdirTemp(parent, ".source-*")
		if err != nil {
			return fmt.Errorf("mktemp source dir: %w", err)
		}
		initArgs := []string{"init", "--bare"}
		if len(requirement.RequiredCommit) == 64 {
			initArgs = append(initArgs, "--object-format=sha256")
		}
		initArgs = append(initArgs, candidateGitDir)
		if _, err := runGitWithEnv(ctx, "", env, initArgs...); err != nil {
			return err
		}
		// The Git directory is exposed through the mount's .git file, so Git
		// must treat callers in the mounted directory as worktree operations.
		if _, err := runGitWithEnv(ctx, candidateGitDir, env, "config", "core.bare", "false"); err != nil {
			return err
		}
		if _, err := runGitWithEnv(ctx, candidateGitDir, env, "remote", "add", "origin", safeURL); err != nil {
			return err
		}
		refspec := "+" + requirement.Ref + ":" + verifiedSourceTrackingRef
		if _, err := runGitWithEnv(ctx, candidateGitDir, env, "config", "--replace-all", "remote.origin.fetch", refspec); err != nil {
			return err
		}
		args := []string{"fetch", "--filter=blob:none", "--no-tags"}
		if requirement.Depth > 0 {
			args = append(args, fmt.Sprintf("--depth=%d", requirement.Depth))
		}
		args = append(args, "origin", refspec)
		if _, err := runGitWithEnv(ctx, candidateGitDir, env, args...); err != nil {
			return err
		}
		observed, err := runGit(ctx, candidateGitDir, "rev-parse", "--verify", verifiedSourceTrackingRef+"^{commit}")
		if err != nil {
			return fmt.Errorf("source ref %s did not resolve to a commit: %w", requirement.Ref, err)
		}
		observed = strings.ToLower(strings.TrimSpace(observed))
		if observed != requirement.RequiredCommit {
			return fmt.Errorf("source changed: %s resolved to %s, required %s", requirement.Ref, observed, requirement.RequiredCommit)
		}
		if err := prepareFixedHEAD(ctx, candidateGitDir, requirement.RequiredCommit, true); err != nil {
			return err
		}
		return nil
	}
	if err := s.retryGitOperation(ctx, GitOperationClone, cfg.Name, attempt); err != nil {
		if candidateGitDir != "" {
			_ = os.RemoveAll(candidateGitDir)
		}
		return model.PreparedSource{}, err
	}
	defer os.RemoveAll(candidateGitDir)
	if err := installGitDir(candidateGitDir, cfg.GitDir); err != nil {
		return model.PreparedSource{}, err
	}
	candidateGitDir = ""
	s.invalidateBatchPool(cfg.GitDir)
	prepared.Acquired = true
	return prepared, nil
}

func cloneExactRef(ctx context.Context, target string, safeURL string, env []string, ref string, depth int) error {
	advertisement, err := runGitWithEnv(ctx, "", env, "ls-remote", safeURL, ref)
	if err != nil {
		return err
	}
	advertisedOID := ""
	for line := range strings.SplitSeq(advertisement, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			advertisedOID = strings.ToLower(fields[0])
			break
		}
	}
	if advertisedOID == "" {
		return fmt.Errorf("source ref %s was not advertised by the remote", ref)
	}
	initArgs := []string{"init"}
	switch len(advertisedOID) {
	case 40:
	case 64:
		initArgs = append(initArgs, "--object-format=sha256")
	default:
		return fmt.Errorf("source ref %s advertised an unsupported object ID", ref)
	}
	initArgs = append(initArgs, target)
	if _, err := runGitWithEnv(ctx, "", env, initArgs...); err != nil {
		return err
	}
	gitDir := filepath.Join(target, ".git")
	if _, err := runGitWithEnv(ctx, gitDir, env, "remote", "add", "origin", safeURL); err != nil {
		return err
	}
	refspec := "+" + ref + ":" + fetchedFullRefRemoteTrackingRef
	if _, err := runGitWithEnv(ctx, gitDir, env, "config", "--replace-all", "remote.origin.fetch", refspec); err != nil {
		return err
	}
	fetchArgs := []string{"fetch", "--filter=blob:none", "--no-tags"}
	if depth > 0 {
		fetchArgs = append(fetchArgs, fmt.Sprintf("--depth=%d", depth))
	}
	fetchArgs = append(fetchArgs, "origin", refspec)
	if _, err := runGitWithEnv(ctx, gitDir, env, fetchArgs...); err != nil {
		return err
	}
	commit, err := runGit(ctx, gitDir, "rev-parse", "--verify", fetchedFullRefRemoteTrackingRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("source ref %s did not resolve to a commit: %w", ref, err)
	}
	_, err = runGit(ctx, gitDir, "update-ref", "--no-deref", "HEAD", strings.TrimSpace(commit))
	return err
}

func requiredCommitAvailable(ctx context.Context, gitDir string, commit string) (bool, error) {
	resolved, err := runGit(ctx, gitDir, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(resolved), commit) {
		return false, nil
	}
	index, err := os.Open(filepath.Join(gitDir, "index"))
	if err != nil {
		return false, nil
	}
	defer index.Close()
	header := make([]byte, 4)
	if _, err := io.ReadFull(index, header); err != nil {
		return false, nil
	}
	return string(header) == "DIRC", nil
}

func prepareFixedHEAD(ctx context.Context, gitDir string, commit string, resetIndex bool) error {
	resolved, err := runGit(ctx, gitDir, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return fmt.Errorf("required commit %s is unavailable: %w", commit, err)
	}
	if !strings.EqualFold(strings.TrimSpace(resolved), commit) {
		return fmt.Errorf("required commit %s resolved unexpectedly", commit)
	}
	if _, err := runGit(ctx, gitDir, "update-ref", "--no-deref", "HEAD", commit); err != nil {
		return err
	}
	if !resetIndex {
		return nil
	}
	_, err = runGit(ctx, gitDir, "read-tree", commit)
	return err
}

func installGitDir(candidate string, destination string) error {
	if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
		return os.Rename(candidate, destination)
	} else if err != nil {
		return err
	}
	backup, err := os.MkdirTemp(filepath.Dir(destination), ".replaced-*")
	if err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	if err := os.Rename(destination, backup); err != nil {
		return err
	}
	if err := os.Rename(candidate, destination); err != nil {
		_ = os.Rename(backup, destination)
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}

func (s *Store) invalidateBatchPool(gitDir string) {
	s.mu.Lock()
	pool := s.pools[gitDir]
	delete(s.pools, gitDir)
	s.mu.Unlock()
	if pool != nil {
		pool.closeAll()
	}
}

func (s *Store) retryGitOperation(ctx context.Context, operation GitOperation, repoName string, attempt func() error) error {
	return s.retryGitOperationForRemotes(ctx, operation, repoName, nil, attempt)
}

func (s *Store) retryGitOperationForRemotes(ctx context.Context, operation GitOperation, repoName string, remotes []string, attempt func() error) error {
	return s.retryGitOperationForRemoteLookup(ctx, operation, repoName, func() ([]string, bool) {
		return remotes, true
	}, attempt)
}

func (s *Store) retryGitOperationForRemoteLookup(ctx context.Context, operation GitOperation, repoName string, remotesForLogging func() ([]string, bool), attempt func() error) error {
	delays := s.gitRetryDelays
	var lastErr *GitOperationError
	operationStarted := time.Now()
	safeRepoName := auth.RedactLogString(repoName)
	logger := s.logger.With("operation", operation, "repo", safeRepoName)
	logInterrupted := func(operationErr *GitOperationError, contextErr error) {
		operationErr.contextErr = contextErr
		args := []any{"attempts", operationErr.Attempts, "duration_ms", time.Since(operationStarted).Milliseconds(), "timed_out", errors.Is(contextErr, context.DeadlineExceeded), "canceled", errors.Is(contextErr, context.Canceled), "error", auth.RedactLogString(operationErr.Error())}
		if errors.Is(contextErr, context.Canceled) {
			logger.InfoContext(ctx, logGitOperationInterrupted, args...)
		} else {
			logger.WarnContext(ctx, logGitOperationInterrupted, args...)
		}
	}
	for n := 0; ; n++ {
		if contextErr := ctx.Err(); contextErr != nil {
			if lastErr != nil {
				logInterrupted(lastErr, contextErr)
				return lastErr
			}
			return &GitOperationError{Operation: operation, Attempts: n, contextErr: contextErr}
		}

		attemptStarted := time.Now()
		err := attempt()
		if err == nil {
			if n > 0 {
				logger.InfoContext(ctx, logGitOperationRecovered, "attempts", n+1, "duration_ms", time.Since(operationStarted).Milliseconds())
			}
			return nil
		}
		cause, contextErr, retryable := gitOperationCauses(err)
		if contextErr == nil {
			contextErr = ctx.Err()
		}
		if cause != nil {
			remotes, complete := remotesForLogging()
			message := "git command failed"
			if complete {
				message = auth.RedactLogString(cause.Error(), remotes...)
			}
			cause = &redactedCause{message: message, cause: cause}
		}
		operationErr := &GitOperationError{
			Operation:  operation,
			Attempts:   n + 1,
			Retryable:  retryable,
			cause:      cause,
			contextErr: contextErr,
		}
		lastErr = operationErr
		safeError := ""
		if cause != nil {
			safeError = auth.RedactLogString(cause.Error())
		} else if contextErr != nil {
			safeError = auth.RedactLogString(contextErr.Error())
		} else {
			safeError = auth.RedactLogString(operationErr.Error())
		}
		logger.WarnContext(ctx, logGitOperationAttemptFailed,
			"attempt", n+1,
			"max_attempts", len(delays)+1,
			"retryable", retryable,
			"duration_ms", time.Since(attemptStarted).Milliseconds(),
			"timed_out", errors.Is(contextErr, context.DeadlineExceeded),
			"canceled", errors.Is(contextErr, context.Canceled),
			"error", safeError,
		)
		if contextErr != nil {
			return operationErr
		}

		if !retryable || n == len(delays) {
			return operationErr
		}
		delay := equalJitter(delays[n])
		logger.InfoContext(ctx, logGitOperationRetrying, "attempt", n+1, "next_attempt", n+2, "backoff_ms", delay.Milliseconds())
		if waitErr := waitForGitRetry(ctx, delay); waitErr != nil {
			logInterrupted(operationErr, waitErr)
			return operationErr
		}
	}
}

func gitOperationCauses(err error) (cause error, contextErr error, retryable bool) {
	var commandErr *gitCommandError
	if errors.As(err, &commandErr) {
		return commandErr.cause, commandErr.contextErr, commandErr.retryable
	}
	return err, nil, isTransientGitError(err)
}

func equalJitter(maximum time.Duration) time.Duration {
	if maximum <= 1 {
		return maximum
	}
	half := maximum / 2
	return half + time.Duration(rand.Int64N(int64(maximum-half)))
}

func waitForGitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isTransientGitError(err error) bool {
	if err == nil {
		return false
	}
	return isTransientGitMessage(err.Error())
}

var permanentGitFailureSymptoms = []string{
	"no space left on device",
	"disk quota exceeded",
	"permission denied",
	"read-only file system",
}

var transientGitFailureSymptoms = []string{
	"http 408", "http/1.0 408", "http/1.1 408", "http/2 408", "returned error: 408",
	"http 500", "http/1.0 500", "http/1.1 500", "http/2 500", "returned error: 500",
	"http 502", "http/1.0 502", "http/1.1 502", "http/2 502", "returned error: 502",
	"http 503", "http/1.0 503", "http/1.1 503", "http/2 503", "returned error: 503",
	"http 504", "http/1.0 504", "http/1.1 504", "http/2 504", "returned error: 504",
	"http 522", "http/1.0 522", "http/1.1 522", "http/2 522", "returned error: 522",
	"http 524", "http/1.0 524", "http/1.1 524", "http/2 524", "returned error: 524",
	"curl 18 ",
	"curl 55 ",
	"curl 56 ",
	"curl 92 ",
	"unexpected disconnect",
	"remote end hung up unexpectedly",
	"remote hung up",
	"connection reset",
	"connection timed out",
	"could not resolve host",
	"couldn't connect to server",
	"failed to connect to",
	"operation timed out",
	"timeout was reached",
	"empty reply from server",
	"transfer closed with",
	"send failure: broken pipe",
	"tls connection was non-properly terminated",
	"was not closed cleanly: cancel",
	"reset by server",
	"bytes of body are still expected",
	"bytes of length header were received",
	"early eof",
}

func containsGitSymptom(message string, symptoms []string) bool {
	for _, symptom := range symptoms {
		if strings.Contains(message, symptom) {
			return true
		}
	}
	return false
}

func isTransientGitMessage(message string) bool {
	message = strings.ToLower(message)
	// Generic index-pack summaries can follow local failures that retrying cannot repair.
	return !containsGitSymptom(message, permanentGitFailureSymptoms) && containsGitSymptom(message, transientGitFailureSymptoms)
}

func (s *Store) Fetch(ctx context.Context, repo model.RepoConfig) error {
	remotesForLogging, cancelLookup := s.startRemotesForLogging(ctx, repo)
	defer cancelLookup()
	return s.retryGitOperationForRemoteLookup(ctx, GitOperationFetch, repo.Name, remotesForLogging, func() error {
		args := []string{"fetch", "--no-tags"}
		if repo.HistoryDepth > 0 {
			args = append(args, fmt.Sprintf("--depth=%d", repo.HistoryDepth))
		}
		args = append(args, "origin")
		_, err := runGit(ctx, repo.GitDir, args...)
		return err
	})
}

func (s *Store) FetchRefNonInteractive(ctx context.Context, repo model.RepoConfig, ref string) error {
	return s.fetchRef(ctx, repo, ref, nonInteractiveGitEnv())
}

func (s *Store) FetchRefWithCredentials(ctx context.Context, repo model.RepoConfig, ref string) error {
	_, env, err := credentialEnv(repo.RemoteURL)
	if err != nil {
		return err
	}
	return s.fetchRef(ctx, repo, ref, env)
}

func (s *Store) fetchRef(ctx context.Context, repo model.RepoConfig, ref string, env []string) error {
	target, err := fetchRefTarget(repo, ref)
	if err != nil {
		return err
	}
	refspec := "+" + target.sourceRef + ":" + target.remoteRef
	if target.branch != "" {
		refspec = fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", target.branch, target.branch)
	}
	remotesForLogging, cancelLookup := s.startRemotesForLogging(ctx, repo)
	defer cancelLookup()
	return s.retryGitOperationForRemoteLookup(ctx, GitOperationFetch, repo.Name, remotesForLogging, func() error {
		_, err := runGitWithEnv(ctx, repo.GitDir, env, "fetch", "--filter=blob:none", "--no-tags", "origin", refspec)
		return err
	})
}

func (s *Store) startRemotesForLogging(ctx context.Context, repo model.RepoConfig) (func() ([]string, bool), context.CancelFunc) {
	type lookupResult struct {
		remote string
		err    error
	}
	lookupCtx, cancel := context.WithCancel(ctx)
	result := make(chan lookupResult, 1)
	go func() {
		remote, err := runGit(lookupCtx, repo.GitDir, "remote", "get-url", "origin")
		result <- lookupResult{remote: strings.TrimSpace(remote), err: err}
	}()

	finished := false
	known := false
	actualRemote := ""
	return func() ([]string, bool) {
		if !finished {
			select {
			case lookup := <-result:
				finished = true
				known = lookup.err == nil
				actualRemote = lookup.remote
			default:
			}
		}
		remotes := []string{repo.RemoteURL}
		if known && actualRemote != "" && actualRemote != strings.TrimSpace(repo.RemoteURL) {
			remotes = append(remotes, actualRemote)
		}
		return remotes, known
	}, cancel
}

func (s *Store) PrepareExistingCloneNonInteractive(ctx context.Context, repo model.RepoConfig) error {
	if err := s.ValidateAmbientRemote(repo); err != nil {
		return err
	}
	if err := s.ConfigureRemoteNonInteractive(ctx, repo); err != nil {
		return err
	}
	if err := s.FetchRefNonInteractive(ctx, repo, repo.FetchRef); err != nil {
		return err
	}
	return s.PrepareFetchedBranch(ctx, repo, repo.FetchRef)
}

func (s *Store) ConfigureRemoteNonInteractive(ctx context.Context, repo model.RepoConfig) error {
	return s.configureRemote(ctx, repo, repo.RemoteURL, nonInteractiveGitEnv())
}

func (s *Store) ConfigureRemoteWithCredentials(ctx context.Context, repo model.RepoConfig) error {
	safeURL, env, err := credentialEnv(repo.RemoteURL)
	if err != nil {
		return err
	}
	return s.configureRemote(ctx, repo, safeURL, env)
}

func (s *Store) configureRemote(ctx context.Context, repo model.RepoConfig, targetRemote string, env []string) error {
	actualRemote, err := runGit(ctx, repo.GitDir, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if strings.TrimSpace(actualRemote) != strings.TrimSpace(targetRemote) {
		if _, err := runGitWithEnv(ctx, repo.GitDir, env, "remote", "set-url", "origin", targetRemote); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ValidateAmbientRemote(repo model.RepoConfig) error {
	if strings.TrimSpace(repo.RemoteURL) == "" {
		return errors.New("remote URL is required")
	}
	safeURL, _, err := credentialEnv(repo.RemoteURL)
	if err != nil {
		return err
	}
	if safeURL != repo.RemoteURL {
		return errors.New("remote must use ambient credentials")
	}
	return nil
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
	oldOID, err := currentOrEmptyTreeOID(ctx, repo.GitDir)
	if err != nil {
		return err
	}
	if target.branch == "" {
		if err := readTreeTransition(ctx, repo.GitDir, oldOID, oid); err != nil {
			return err
		}
		oldHeadOID, err := refOIDOrZero(ctx, repo.GitDir, "HEAD")
		if err != nil {
			return rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
		}
		updateArgs := []string{"update-ref", "--no-deref", "HEAD", oid}
		if strings.Trim(oldHeadOID, "0") != "" {
			updateArgs = append(updateArgs, oldHeadOID)
		}
		if _, err := runGit(ctx, repo.GitDir, updateArgs...); err != nil {
			return rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
		}
		return nil
	}
	refName := "refs/heads/" + target.branch
	var oldBranchOID string
	if repo.PreparedGitDir {
		oldBranchOID, err = s.preparedBranchExpectedOID(ctx, repo, target.branch, oid)
	} else {
		oldBranchOID, err = refOIDOrZero(ctx, repo.GitDir, refName)
	}
	if err != nil {
		return err
	}
	if err := readTreeTransition(ctx, repo.GitDir, oldOID, oid); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, "update-ref", refName, oid, oldBranchOID); err != nil {
		return rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
	}
	if _, err := runGit(ctx, repo.GitDir, "symbolic-ref", "HEAD", refName); err != nil {
		refErr := restoreRef(ctx, repo.GitDir, refName, oldBranchOID, oid)
		indexErr := rollbackIndexTransition(ctx, repo.GitDir, oid, oldOID, err)
		return errors.Join(indexErr, refErr)
	}
	if _, err := runGit(ctx, repo.GitDir, "branch", "--set-upstream-to", "origin/"+target.branch, target.branch); err != nil {
		s.logger.WarnContext(ctx, "set upstream failed", "repo", repo.Name, "error", err)
	}
	return nil
}

func refOIDOrZero(ctx context.Context, gitDir string, refName string) (string, error) {
	oid, err := runGit(ctx, gitDir, "rev-parse", "--verify", refName+"^{commit}")
	if err == nil {
		return strings.TrimSpace(oid), nil
	}
	return zeroOIDForGitDir(ctx, gitDir)
}

func rollbackIndexTransition(ctx context.Context, gitDir string, fromOID string, toOID string, cause error) error {
	if err := readTreeTransition(ctx, gitDir, fromOID, toOID); err != nil {
		return errors.Join(cause, fmt.Errorf("restore index after ref update failure: %w", err))
	}
	return cause
}

func restoreRef(ctx context.Context, gitDir string, refName string, oldOID string, newOID string) error {
	if _, err := runGit(ctx, gitDir, "update-ref", refName, oldOID, newOID); err != nil {
		return fmt.Errorf("restore ref after HEAD update failure: %w", err)
	}
	return nil
}

func currentOrEmptyTreeOID(ctx context.Context, gitDir string) (string, error) {
	oid, headErr := runGit(ctx, gitDir, "rev-parse", "--verify", "HEAD^{commit}")
	if headErr == nil {
		return strings.TrimSpace(oid), nil
	}
	if _, err := runGit(ctx, gitDir, "symbolic-ref", "-q", "HEAD"); err != nil {
		return "", headErr
	}
	oid, err := runGit(ctx, gitDir, "hash-object", "-t", "tree", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(oid), nil
}

func readTreeTransition(ctx context.Context, gitDir string, oldOID string, newOID string) error {
	_, err := runGit(ctx, gitDir, "read-tree", "-m", oldOID, newOID)
	return err
}

func (s *Store) preparedBranchExpectedOID(ctx context.Context, repo model.RepoConfig, branch string, oid string) (string, error) {
	current, err := runGit(ctx, repo.GitDir, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return zeroOIDForGitDir(ctx, repo.GitDir)
	}
	current = strings.TrimSpace(current)
	if current == oid {
		return current, nil
	}
	if _, err := runGit(ctx, repo.GitDir, "merge-base", "--is-ancestor", current, oid); err != nil {
		return "", fmt.Errorf("prepared git dir branch %s would be overwritten; refusing non-fast-forward update", branch)
	}
	return current, nil
}

func zeroOIDForGitDir(ctx context.Context, gitDir string) (string, error) {
	format, err := runGit(ctx, gitDir, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(format) {
	case "sha1":
		return strings.Repeat("0", 40), nil
	case "sha256":
		return strings.Repeat("0", 64), nil
	default:
		return "", fmt.Errorf("unsupported Git object format %q", format)
	}
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
	nodes := []model.BaseNode{rootNode(repo.ID)}
	var blobOIDs []string
	blobIndex := map[string][]int{} // oid -> indices into nodes
	var parseErr error
	if err := streamTreeRecords(ctx, repo.GitDir, headOID, func(line string) {
		n, typ, ok := parseTreeRecord(repo.ID, line)
		if !ok {
			if typ != "commit" && parseErr == nil {
				parseErr = fmt.Errorf("invalid ls-tree record %q", line)
			}
			return
		}
		idx := len(nodes)
		nodes = append(nodes, n)
		if typ == "blob" && n.ObjectOID != "" {
			blobIndex[n.ObjectOID] = append(blobIndex[n.ObjectOID], idx)
			if len(blobIndex[n.ObjectOID]) == 1 {
				blobOIDs = append(blobOIDs, n.ObjectOID)
			}
		}
	}); err != nil {
		return nil, err
	}
	if parseErr != nil {
		return nil, parseErr
	}

	// Batch-resolve sizes using cat-file --batch-check. This reads from local
	// pack metadata and doesn't trigger network fetches on blobless clones.
	if err := s.batchResolveSizes(ctx, repo, nodes, blobOIDs, blobIndex); err != nil {
		// Non-fatal: sizes remain "unknown" and reads will still work via
		// hydration. Log so operators can diagnose unexpected attr hydration.
		s.logger.Warn("batch size resolution failed, some file sizes will resolve on demand", "repo", repo.Name, "error", err)
	}
	return addImplicitDirs(repo.ID, nodes), nil
}

func streamTreeRecords(ctx context.Context, gitDir string, headOID string, fn func(string)) error {
	cmd := exec.CommandContext(ctx, "git", "ls-tree", "-r", "-t", "-z", headOID)
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errBuf := &bytes.Buffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		return err
	}
	readErr := readNullDelimited(stdout, fn)
	waitErr := cmd.Wait()
	if readErr != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return errors.Join(readErr, contextErr)
		}
		return readErr
	}
	if waitErr != nil {
		msg := auth.RedactString(strings.TrimSpace(errBuf.String()))
		if msg == "" {
			msg = auth.RedactString(waitErr.Error())
		}
		if contextErr := ctx.Err(); contextErr != nil {
			if strings.EqualFold(msg, "signal: killed") {
				return contextErr
			}
			return errors.Join(errors.New(msg), contextErr)
		}
		return errors.New(msg)
	}
	return nil
}

func readNullDelimited(r io.Reader, fn func(string)) error {
	reader := bufio.NewReader(r)
	for {
		record, err := reader.ReadString('\x00')
		if err == nil && record != "" {
			record = strings.TrimSuffix(record, "\x00")
			if record != "" {
				fn(record)
			}
		}
		if errors.Is(err, io.EOF) {
			if record != "" {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func parseTreeRecord(repoID model.RepoID, line string) (model.BaseNode, string, bool) {
	parts := strings.SplitN(line, "\t", 2)
	if len(parts) != 2 {
		return model.BaseNode{}, "", false
	}
	meta := strings.Fields(parts[0])
	if len(meta) < 3 {
		return model.BaseNode{}, "", false
	}
	modeStr := meta[0]
	typ := meta[1]
	oid := meta[2]
	mode64, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		return model.BaseNode{}, typ, false
	}
	mode := uint32(mode64)
	if typ == "commit" {
		return model.BaseNode{}, typ, false
	}
	return model.BaseNode{
		RepoID:    repoID,
		Path:      parts[1],
		Type:      normalizeGitType(typ, mode),
		Mode:      mode,
		ObjectOID: oid,
		SizeState: "unknown",
		SizeBytes: 0,
	}, typ, true
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errBuf := &bytes.Buffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		return err
	}
	writeErrCh := make(chan error, 1)
	go func() {
		var writeErr error
		for _, oid := range oids {
			if _, writeErr = fmt.Fprintln(stdin, oid); writeErr != nil {
				break
			}
		}
		if closeErr := stdin.Close(); writeErr == nil {
			writeErr = closeErr
		}
		writeErrCh <- writeErr
	}()
	// Output format: "<oid> <type> <size>" or "<oid> missing"
	scan := bufio.NewScanner(stdout)
	for scan.Scan() {
		applyBatchCheckLine(nodes, index, scan.Text())
	}
	scanErr := scan.Err()
	writeErr := <-writeErrCh
	waitErr := cmd.Wait()
	if writeErr != nil {
		return writeErr
	}
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		msg := auth.RedactString(strings.TrimSpace(errBuf.String()))
		if msg == "" {
			msg = auth.RedactString(waitErr.Error())
		}
		return errors.New(msg)
	}
	return nil
}

func applyBatchCheckLine(nodes []model.BaseNode, index map[string][]int, line string) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return
	}
	oid := fields[0]
	sizeStr := fields[2]
	sz, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return
	}
	for _, idx := range index[oid] {
		nodes[idx].SizeBytes = sz
		nodes[idx].SizeState = "known"
	}
}

// BlobToCache fetches a git object and writes it to dstPath in a binary-safe manner.
// Uses a persistent cat-file --batch process to amortize process spawn and
// remote connection costs across multiple blob fetches.
func (s *Store) BlobToCache(ctx context.Context, repo model.RepoConfig, objectOID string, dstPath string) (size int64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return 0, err
	}
	pool, err := s.getPool(repo.GitDir)
	if err != nil {
		return 0, err
	}
	batch, err := pool.acquire(ctx)
	if err != nil {
		return 0, err
	}
	size, err = fetchBatchToFile(ctx, batch, objectOID, dstPath)
	if err != nil {
		pool.discard(batch)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, err
		}
		// Process may have died or be desynchronized; discard and retry.
		batch, err = pool.acquire(ctx)
		if err != nil {
			return 0, err
		}
		size, err = fetchBatchToFile(ctx, batch, objectOID, dstPath)
		if err != nil {
			pool.discard(batch)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return 0, err
			}
			// Retry also failed; close instead of returning a potentially
			// corrupted process to the pool.
			return 0, err
		}
	}
	pool.release(batch)
	return size, err
}

func fetchBatchToFile(ctx context.Context, batch *batchCatFile, objectOID string, dstPath string) (int64, error) {
	ch := make(chan fetchBlobResult, 1)
	go func() {
		size, err := batch.fetchToFile(objectOID, dstPath)
		ch <- fetchBlobResult{size: size, err: err}
	}()
	select {
	case r := <-ch:
		return r.size, r.err
	case <-ctx.Done():
		batch.kill()
		<-ch
		return 0, ctx.Err()
	}
}

func (s *Store) ReadBlob(ctx context.Context, repo model.RepoConfig, objectOID string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("negative max bytes: %d", maxBytes)
	}
	pool, err := s.getPool(repo.GitDir)
	if err != nil {
		return nil, err
	}
	batch, err := pool.acquire(ctx)
	if err != nil {
		return nil, err
	}
	data, err := readBatchBlob(ctx, batch, objectOID, maxBytes)
	if err == nil {
		pool.release(batch)
		return data, nil
	}
	if errors.Is(err, model.ErrBlobTooLarge) {
		pool.discard(batch)
		return nil, err
	}
	pool.discard(batch)

	batch, err = pool.acquire(ctx)
	if err != nil {
		return nil, err
	}
	data, err = readBatchBlob(ctx, batch, objectOID, maxBytes)
	if err != nil {
		if errors.Is(err, model.ErrBlobTooLarge) {
			pool.discard(batch)
			return nil, err
		}
		pool.discard(batch)
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

func (s *Store) getPool(gitDir string) (*batchPool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("git store closed")
	}
	if p, ok := s.pools[gitDir]; ok {
		return p, nil
	}
	p := &batchPool{gitDir: gitDir, logger: s.logger, maxSize: s.poolMaxSize, all: map[*batchCatFile]struct{}{}, changed: make(chan struct{})}
	s.pools[gitDir] = p
	return p, nil
}

// batchPool maintains a pool of reusable cat-file --batch processes so
// multiple hydrator workers can fetch blobs concurrently.
type batchPool struct {
	mu       sync.Mutex
	free     []*batchCatFile
	gitDir   string
	logger   *slog.Logger
	maxSize  int
	creating int
	all      map[*batchCatFile]struct{}
	closed   bool
	changed  chan struct{}
}

func (p *batchPool) acquire(ctx context.Context) (*batchCatFile, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("git batch pool closed")
		}
		if n := len(p.free); n > 0 {
			b := p.free[n-1]
			p.free = p.free[:n-1]
			p.mu.Unlock()
			if b.alive() {
				return b, nil
			}
			p.discard(b)
			continue
		}
		if len(p.all)+p.creating < p.maxSize {
			p.creating++
			p.mu.Unlock()
			b, err := newBatchCatFile(p.gitDir, p.logger)
			p.mu.Lock()
			p.creating--
			if err == nil && !p.closed {
				p.all[b] = struct{}{}
			}
			closed := p.closed
			p.signalLocked()
			p.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if closed {
				b.kill()
				return nil, errors.New("git batch pool closed")
			}
			return b, nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (p *batchPool) release(b *batchCatFile) {
	if !b.alive() {
		p.discard(b)
		return
	}
	p.mu.Lock()
	if !p.closed && len(p.all) <= p.maxSize {
		p.free = append(p.free, b)
		p.signalLocked()
		p.mu.Unlock()
		return
	}
	delete(p.all, b)
	p.signalLocked()
	p.mu.Unlock()
	b.close()
}

func (p *batchPool) discard(b *batchCatFile) {
	p.mu.Lock()
	if _, ok := p.all[b]; ok {
		delete(p.all, b)
		p.signalLocked()
	}
	p.mu.Unlock()
	b.kill()
}

func (p *batchPool) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *batchPool) closeAll() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	all := make([]*batchCatFile, 0, len(p.all))
	for b := range p.all {
		all = append(all, b)
	}
	p.all = map[*batchCatFile]struct{}{}
	p.free = nil
	p.signalLocked()
	p.mu.Unlock()
	for _, b := range all {
		b.kill()
	}
}

func (p *batchPool) setMaxSize(n int) {
	var extras []*batchCatFile
	p.mu.Lock()
	p.maxSize = n
	for len(p.all) > n && len(p.free) > 0 {
		idx := len(p.free) - 1
		b := p.free[idx]
		p.free = p.free[:idx]
		delete(p.all, b)
		extras = append(extras, b)
	}
	p.signalLocked()
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
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdoutPipe io.ReadCloser
	stdout     *bufio.Reader
	logger     *slog.Logger
	closeOnce  sync.Once
}

func newBatchCatFile(gitDir string, logger *slog.Logger) (*batchCatFile, error) {
	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir)
	cmd.Stderr = os.Stderr
	configureCommandProcessGroup(cmd)

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
		cmd:        cmd,
		stdin:      stdin,
		stdoutPipe: stdout,
		stdout:     bufio.NewReaderSize(stdout, 256*1024),
		logger:     logger,
	}, nil
}

func (b *batchCatFile) alive() bool {
	return b.cmd != nil && b.cmd.Process != nil && b.cmd.ProcessState == nil
}

func (b *batchCatFile) close() {
	b.shutdown(false)
}

func (b *batchCatFile) kill() {
	b.shutdown(true)

}

func (b *batchCatFile) shutdown(kill bool) {
	b.closeOnce.Do(func() {
		if kill {
			if b.stdoutPipe != nil {
				_ = b.stdoutPipe.Close()
			}
			_ = killCommandProcessGroup(b.cmd)
		}
		if b.stdin != nil {
			_ = b.stdin.Close()
		}
		if b.cmd != nil && b.cmd.Process != nil {
			_ = b.cmd.Wait()
		}
	})
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
	f, err := os.CreateTemp(filepath.Dir(dstPath), ".artifact-fs-blob-*")
	if err != nil {
		// Drain the blob content so the protocol stays in sync.
		_, _ = io.CopyN(io.Discard, b.stdout, size+1) // +1 for trailing LF
		return 0, err
	}
	tmp := f.Name()
	written, copyErr := io.CopyN(f, b.stdout, size)
	// Read the trailing LF that git appends after the content. If this fails
	// the batch protocol is desynchronized and the caller must discard the
	// process.
	if lf, lfErr := b.stdout.ReadByte(); copyErr == nil {
		if lfErr != nil {
			copyErr = fmt.Errorf("batch read trailing LF: %w", lfErr)
		} else if lf != '\n' {
			copyErr = fmt.Errorf("batch read trailing byte: got %#x, want newline", lf)
		}
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
	lf, err := b.stdout.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("batch read trailing LF: %w", err)
	}
	if lf != '\n' {
		return nil, fmt.Errorf("batch read trailing byte: got %#x, want newline", lf)
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
	if fields[0] != oid {
		return 0, fmt.Errorf("unexpected batch object: got %q, want %q", fields[0], oid)
	}
	if fields[1] == "missing" {
		return 0, fmt.Errorf("object %s missing", oid)
	}
	if len(fields) < 3 {
		return 0, fmt.Errorf("unexpected batch header: %q", header)
	}
	if fields[1] != "blob" {
		return 0, fmt.Errorf("unexpected batch object type: %q", fields[1])
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

// ReadTreeHEAD initializes the index during controlled clone or preparation.
// Do not call it from delayed HEAD watchers: it can discard newer staged work.
func (s *Store) ReadTreeHEAD(ctx context.Context, repo model.RepoConfig) error {
	_, err := runGit(ctx, repo.GitDir, "read-tree", "HEAD")
	return err
}

// EnsureIndexInitialized creates a missing index without replacing staged work.
func (s *Store) EnsureIndexInitialized(ctx context.Context, repo model.RepoConfig) error {
	oid, err := runGit(ctx, repo.GitDir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	return readTreeTransition(ctx, repo.GitDir, strings.TrimSpace(oid), strings.TrimSpace(oid))
}

func (s *Store) ConfigureStatusOptimization(ctx context.Context, repo model.RepoConfig, stateRoot string) error {
	if repo.RequiredCommit != "" && strings.TrimSpace(repo.MountPath) != "" {
		if _, err := os.Stat(repo.MountPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		// Verified acquisitions use a standalone Git directory. Pin it only
		// after FUSE mounts so index refreshes cannot infer the caller's cwd.
		if _, err := runGit(ctx, repo.GitDir, "config", "core.worktree", repo.MountPath); err != nil {
			return err
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	hookDir := filepath.Join(repo.GitDir, "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		return err
	}
	hookPath := filepath.Join(hookDir, "artifact-fs-fsmonitor")
	script := fsmonitorHookScript(stateRoot, exe, repo.Name)
	if err := os.WriteFile(hookPath, []byte(script), 0o755); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, "config", "core.fsmonitor", hookPath); err != nil {
		return err
	}
	if _, err := runGit(ctx, repo.GitDir, "config", "fsmonitor.allowRemote", "true"); err != nil {
		return err
	}
	workTreeEnv := gitWorkTreeEnv(repo.MountPath)
	if _, err := runGitWithEnv(ctx, repo.GitDir, workTreeEnv, "update-index", "--fsmonitor"); err != nil {
		return err
	}
	return markIndexFSMonitorValid(ctx, repo.GitDir, repo.MountPath)
}

func gitWorkTreeEnv(workTree string) []string {
	if strings.TrimSpace(workTree) == "" {
		return nil
	}
	return []string{"GIT_WORK_TREE=" + workTree}
}

func markIndexFSMonitorValid(ctx context.Context, gitDir, workTree string) error {
	env := append(os.Environ(), "GIT_DIR="+gitDir)
	env = append(env, gitWorkTreeEnv(workTree)...)
	ls := exec.CommandContext(ctx, "git", "ls-files", "-z")
	ls.Env = env
	stdout, err := ls.StdoutPipe()
	if err != nil {
		return err
	}
	lsErr := &bytes.Buffer{}
	ls.Stderr = lsErr
	update := exec.CommandContext(ctx, "git", "update-index", "--fsmonitor-valid", "-z", "--stdin")
	update.Env = env
	update.Stdin = stdout
	updateErr := &bytes.Buffer{}
	update.Stderr = updateErr
	if err := ls.Start(); err != nil {
		return err
	}
	if err := update.Start(); err != nil {
		_ = ls.Process.Kill()
		_ = ls.Wait()
		return err
	}
	upErr := update.Wait()
	lsWaitErr := ls.Wait()
	if lsWaitErr != nil {
		msg := auth.RedactString(strings.TrimSpace(lsErr.String()))
		if msg == "" {
			msg = auth.RedactString(lsWaitErr.Error())
		}
		return errors.New(msg)
	}
	if upErr != nil {
		msg := auth.RedactString(strings.TrimSpace(updateErr.String()))
		if msg == "" {
			msg = auth.RedactString(upErr.Error())
		}
		return errors.New(msg)
	}
	return nil
}

func (s *Store) ComputeAheadBehind(ctx context.Context, repo model.RepoConfig) (ahead int, behind int, diverged bool, err error) {
	branch := branchName(repo.Branch)
	if branch == "" {
		return 0, 0, false, nil
	}
	rangeSpec := fmt.Sprintf("HEAD...origin/%s", branch)
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
	configureCommandProcessGroup(cmd)
	cmd.Cancel = func() error { return killCommandProcessGroup(cmd) }
	env := os.Environ()
	if gitDir != "" {
		env = append(env, "GIT_DIR="+gitDir)
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	buf := &bytes.Buffer{}
	errBuf := &boundedGitError{limit: maxGitErrorBytes}
	cmd.Stdout = buf
	cmd.Stderr = errBuf
	runErr := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if runErr == nil {
		return out, nil
	}
	contextErr := ctx.Err()

	rawMessage := strings.TrimSpace(errBuf.String())
	retryable := errBuf.Retryable()
	msg := rawMessage
	var credentials []string
	for _, env := range extraEnv {
		for _, prefix := range []string{"ARTIFACT_FS_GIT_USERNAME=", "ARTIFACT_FS_GIT_PASSWORD="} {
			if value, ok := strings.CutPrefix(env, prefix); ok && value != "" {
				credentials = append(credentials, value)
			}
		}
	}
	if len(credentials) == 2 && len(credentials[0]) < len(credentials[1]) {
		credentials[0], credentials[1] = credentials[1], credentials[0]
	}
	for _, credential := range credentials {
		msg = strings.ReplaceAll(msg, credential, "REDACTED")
	}
	msg = redactTruncatedCredentialPrefix(msg, credentials)
	msg = auth.RedactString(msg)
	var cause error
	if msg != "" && !(contextErr != nil && strings.EqualFold(msg, "signal: killed")) {
		cause = errors.New(msg)
	} else if runErr != nil && !(contextErr != nil && strings.EqualFold(strings.TrimSpace(runErr.Error()), "signal: killed")) {
		cause = errors.New(auth.RedactString(runErr.Error()))
	}
	return out, &gitCommandError{cause: cause, contextErr: contextErr, retryable: retryable}
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
		if strings.ToLower(u.Scheme) != "ssh" {
			return "", nil, errors.New("remote URL includes unsupported inline credentials")
		}
		if _, hasPassword := u.User.Password(); hasPassword || auth.HasInlineCredentials(rawURL) {
			return "", nil, errors.New("remote URL includes unsupported inline credentials")
		}
		return rawURL, nil, nil
	}
	if u.Hostname() == "" {
		return "", nil, errors.New("malformed remote URL")
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
	credentialScope := strings.ToLower(u.Scheme) + "://" + u.Host
	return u.String(), []string{
		"GIT_TERMINAL_PROMPT=0",
		"ARTIFACT_FS_GIT_USERNAME=" + credentialUsername,
		"ARTIFACT_FS_GIT_PASSWORD=" + credentialPassword,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=credential." + credentialScope + ".helper",
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
	if schemeLessUserinfoHasPassword(rawURL) {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return auth.HasInlineCredentials(rawURL) || rawUserinfoCandidateHasPassword(rawURL)
	}
	if u.User != nil {
		_, hasPassword := u.User.Password()
		return isHTTPRemote(rawURL, u.Scheme) || strings.ToLower(u.Scheme) != "ssh" || hasPassword || auth.HasInlineCredentials(rawURL)
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
	if isSCPStyleRemote(raw) {
		return false
	}
	if schemeLessUserinfoHasPassword(raw) {
		return true
	}
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

func schemeLessUserinfoHasPassword(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	if isSCPStyleRemote(raw) {
		return false
	}
	end := len(raw)
	if relEnd := strings.IndexAny(raw, "/?#"); relEnd >= 0 {
		end = relEnd
	}
	if end == 0 {
		return false
	}
	prefix := raw[:end]
	at := strings.LastIndex(prefix, "@")
	colon := strings.Index(prefix, ":")
	return colon >= 0 && (at > colon || strings.Contains(raw[end:], "@"))
}

func isSCPStyleRemote(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	end := len(raw)
	if relEnd := strings.IndexAny(raw, "/?#"); relEnd >= 0 {
		end = relEnd
	}
	prefix := raw[:end]
	at := strings.Index(prefix, "@")
	colon := strings.Index(prefix, ":")
	return at > 0 && colon > at
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

func fsmonitorHookScript(stateRoot, exe, repoName string) string {
	return fmt.Sprintf("#!/bin/sh\nARTIFACT_FS_ROOT=%s exec %s fsmonitor-hook --name %s \"$@\"\n", shellScriptQuote(stateRoot), shellScriptQuote(exe), shellScriptQuote(repoName))
}

func shellScriptQuote(s string) string {
	if s == "" {
		return "''"
	}
	if isShellSafeScriptValue(s) {
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

func isShellSafeScriptValue(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if strings.ContainsRune("@%_+=:,./-~", r) {
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
	if after, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return after
	}
	if after, ok := strings.CutPrefix(ref, "refs/remotes/origin/"); ok {
		return after
	}
	if after, ok := strings.CutPrefix(ref, "origin/"); ok {
		return after
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
