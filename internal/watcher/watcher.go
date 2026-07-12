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
	prev            map[string]string
	firstSeenChange map[string]struct{}
}

func New(interval time.Duration) *Poller {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return &Poller{interval: interval, prev: map[string]string{}, firstSeenChange: map[string]struct{}{}}
}

func (p *Poller) Watch(ctx context.Context, gitDir string, fn func()) {
	headPath := filepath.Join(gitDir, "HEAD")
	// Prime first, then reconcile. A change during reconciliation is observed by
	// the next poll instead of falling into the startup baseline window.
	_ = p.headChanged(headPath)
	fn()
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
	line := strings.TrimSpace(string(data))
	ref, ok := strings.CutPrefix(line, "ref: ")
	if !ok {
		return "", false
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	return filepath.Join(filepath.Dir(headPath), filepath.FromSlash(ref)), true
}

func (p *Poller) changed(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	value := string(data)
	prev, ok := p.prev[path]
	p.prev[path] = value
	if !ok {
		if _, changeOnFirstSeen := p.firstSeenChange[path]; changeOnFirstSeen {
			delete(p.firstSeenChange, path)
			return true
		}
		return false
	}
	delete(p.firstSeenChange, path)
	return value != prev
}
