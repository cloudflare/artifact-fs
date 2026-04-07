package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Poller struct {
	interval        time.Duration
	prev            map[string]time.Time
	firstSeenChange map[string]struct{}
}

func New(interval time.Duration) *Poller {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return &Poller{interval: interval, prev: map[string]time.Time{}, firstSeenChange: map[string]struct{}{}}
}

func (p *Poller) Watch(ctx context.Context, gitDir string, fn func()) {
	headPath := filepath.Join(gitDir, "HEAD")
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p.headChanged(headPath) {
				fn()
			}
		}
	}
}

func (p *Poller) headChanged(headPath string) bool {
	if p.changed(headPath) {
		// A HEAD switch selects a different ref path. Prime that ref immediately
		// so the next branch-tip advance is compared against the new baseline.
		if refPath, ok := p.headRefPath(headPath); ok {
			p.primeCurrentRef(refPath)
		}
		return true
	}
	refPath, ok := p.headRefPath(headPath)
	if !ok {
		return false
	}
	return p.currentRefChanged(refPath)
}

func (p *Poller) primeCurrentRef(refPath string) {
	_ = p.currentRefChanged(refPath)
}

func (p *Poller) currentRefChanged(refPath string) bool {
	if _, err := os.Stat(refPath); err != nil {
		p.firstSeenChange[refPath] = struct{}{}
		return false
	}
	return p.changed(refPath)
}

func (p *Poller) headRefPath(headPath string) (string, bool) {
	data, err := os.ReadFile(headPath)
	if err != nil {
		return "", false
	}
	const prefix = "ref: "
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	ref := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if ref == "" {
		return "", false
	}
	return filepath.Join(filepath.Dir(headPath), filepath.FromSlash(ref)), true
}

func (p *Poller) changed(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	mtime := st.ModTime()
	prev, ok := p.prev[path]
	p.prev[path] = mtime
	if !ok {
		if _, changeOnFirstSeen := p.firstSeenChange[path]; changeOnFirstSeen {
			delete(p.firstSeenChange, path)
			return true
		}
		return false
	}
	delete(p.firstSeenChange, path)
	return mtime.After(prev)
}
