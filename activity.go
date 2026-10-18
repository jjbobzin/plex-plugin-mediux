package main

import (
	"sync"
	"time"
)

// activityLog keeps the most recent lookups for the settings page, so you can
// see what Plex asked for and what was found without reading container logs.
type activityLog struct {
	mu      sync.Mutex
	entries []activity
	counts  map[string]int // results since the container started
	started time.Time
}

type activity struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Item   string    `json:"item"`
	Result string    `json:"result"`
	Detail string    `json:"detail"`
}

const activityLimit = 200

func (l *activityLog) add(kind, item, result, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil {
		l.counts = map[string]int{}
	}
	l.counts[result]++
	l.entries = append(l.entries, activity{time.Now(), kind, item, result, detail})
	if len(l.entries) > activityLimit {
		l.entries = l.entries[len(l.entries)-activityLimit:]
	}
}

// recent returns entries newest first.
func (l *activityLog) recent() []activity {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]activity, 0, len(l.entries))
	for i := len(l.entries) - 1; i >= 0; i-- {
		out = append(out, l.entries[i])
	}
	return out
}

// totals returns how many lookups ended in each result since startup.
func (l *activityLog) totals() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for k, v := range l.counts {
		out[k] = v
	}
	return out
}
