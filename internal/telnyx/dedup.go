package telnyx

import (
	"sync"
	"time"
)

// Dedup is a small, non-durable in-memory window of recently seen webhook
// event IDs (ADR-0004). Telnyx may redeliver webhooks; duplicate deliveries of
// the same event ID are ignored. State is lost on restart — accepted per the
// MVP reliability posture.
type Dedup struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time
	seen   map[string]time.Time
}

// NewDedup returns a Dedup that forgets IDs older than window.
func NewDedup(window time.Duration) *Dedup {
	return &Dedup{
		window: window,
		now:    time.Now,
		seen:   make(map[string]time.Time),
	}
}

// Seen reports whether id was seen within the window, recording it either way.
func (d *Dedup) Seen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for k, t := range d.seen {
		if now.Sub(t) > d.window {
			delete(d.seen, k)
		}
	}
	if _, ok := d.seen[id]; ok {
		return true
	}
	d.seen[id] = now
	return false
}
