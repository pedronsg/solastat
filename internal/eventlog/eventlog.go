// Package eventlog is a small, bounded, in-memory activity log for the
// dashboard — core-detected events (battery, comms, inverter) and whatever
// any compiled-in plugin reports, all merged into one feed. Deliberately
// not persisted: it's a fixed-capacity ring buffer, oldest entries fall
// off once full, and it starts empty on every restart. A plugin reports
// what happened via pluginapi.LogEvent; this package timestamps it and
// tracks who reported it — see Entry's own doc comment for how the
// dashboard renders a plugin-sourced entry generically, without knowing
// which plugin it came from.
package eventlog

import (
	"sync"
	"time"
)

// Entry is one logged event, newest-first when read back via Recent.
// Source "core" is the small, fixed set of events the core itself detects
// (battery, comms, inverter fault) — the dashboard knows how to translate
// those by Kind. Any other Source is an opaque plugin ID: the dashboard
// never special-cases a specific plugin, it just displays Text verbatim
// and colors the status dot from OK (true green, false amber) — Text and
// OK are therefore required from every plugin-sourced LogEvent, not
// optional metadata. Kind/On/Code are for the core's own use (or a
// plugin's own page, e.g. relay's activation history) and never
// interpreted here.
type Entry struct {
	Ts     time.Time `json:"ts"`
	Source string    `json:"source,omitempty"`
	Kind   string    `json:"kind,omitempty"`
	On     bool      `json:"on,omitempty"`
	Code   int       `json:"code,omitempty"`
	Text   string    `json:"text,omitempty"`
	// Args are named values a page can put into its own translation of
	// Kind (Text being the English fallback), e.g. {"from", "to"}.
	Args map[string]string `json:"args,omitempty"`
	OK   bool              `json:"ok"`
}

// Log is a fixed-capacity ring buffer of Entry, safe for concurrent use.
type Log struct {
	mu      sync.RWMutex
	entries []Entry
	next    int // index Add will write to next
	count   int // number of valid entries currently held, <= len(entries)
}

// New returns a Log holding at most capacity entries — the "tamanho
// máximo" the caller decides once, at construction. capacity < 1 is
// treated as 1.
func New(capacity int) *Log {
	if capacity < 1 {
		capacity = 1
	}
	return &Log{entries: make([]Entry, capacity)}
}

// Add appends e, overwriting the oldest entry once the log is at capacity.
func (l *Log) Add(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[l.next] = e
	l.next = (l.next + 1) % len(l.entries)
	if l.count < len(l.entries) {
		l.count++
	}
}

// Clear drops every entry.
func (l *Log) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.entries {
		l.entries[i] = Entry{}
	}
	l.next, l.count = 0, 0
}

// Recent returns up to limit entries starting at offset, newest first, and
// the total number of entries currently held (for pagination — this
// shrinks back to 0 across a restart, since nothing is persisted).
func (l *Log) Recent(limit, offset int) ([]Entry, int) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	total := l.count
	if limit <= 0 || offset >= total {
		return []Entry{}, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	cap := len(l.entries)
	out := make([]Entry, 0, end-offset)
	for i := offset; i < end; i++ {
		idx := ((l.next-1-i)%cap + cap) % cap
		out = append(out, l.entries[idx])
	}
	return out, total
}
