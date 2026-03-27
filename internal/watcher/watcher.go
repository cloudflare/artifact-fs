package watcher

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

type Signal struct {
	HEADChanged  bool
	RefsChanged  bool
	IndexChanged bool
}

type Poller struct {
	interval time.Duration
	prev     map[string]time.Time
}

func New(interval time.Duration) *Poller {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return &Poller{interval: interval, prev: map[string]time.Time{}}
}

func (p *Poller) Watch(ctx context.Context, gitDir string, fn func(Signal)) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := Signal{}
			s.HEADChanged = p.changed(filepath.Join(gitDir, "HEAD"))
			s.IndexChanged = p.changed(filepath.Join(gitDir, "index"))
			s.RefsChanged = p.changed(filepath.Join(gitDir, "refs", "heads")) || p.changed(filepath.Join(gitDir, "refs", "remotes"))
			if s.HEADChanged || s.RefsChanged || s.IndexChanged {
				fn(s)
			}
		}
	}
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
		return false
	}
	return mtime.After(prev)
}
