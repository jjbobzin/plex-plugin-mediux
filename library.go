package main

// The Library view: everything in your Plex libraries, merged across
// servers, with what Mediux has for each title. Like AURA, you browse and
// filter your own titles and pick a set from there, so every lookup uses the
// exact ID Plex already has.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type libraryPlace struct {
	Server  string `json:"server"`
	Library string `json:"library"`
	Custom  bool   `json:"custom"` // library is on a Mediux (custom) agent
}

type libraryItem struct {
	Type   mediaType      `json:"type"`
	TMDB   string         `json:"tmdb"`
	Title  string         `json:"title"`
	Year   int            `json:"year,omitempty"`
	Added  int64          `json:"added"`
	Poster string         `json:"poster,omitempty"`
	Places []libraryPlace `json:"places"`
	Pick   string         `json:"pick,omitempty"` // parts picked on this page
	Aura   bool           `json:"aura,omitempty"`
	Art    *int           `json:"art"` // Mediux artwork count; null until checked
}

// library answers GET /api/library (?refresh=1 rereads Plex now).
func (ui *webUI) library(w http.ResponseWriter, r *http.Request) {
	cfg := ui.settings.get()
	if !hasPlexToken(cfg.PlexServers) {
		writeJSON(w, 200, map[string]any{"configured": false, "items": []any{}})
		return
	}
	maxAge := 6 * time.Hour
	if r.URL.Query().Get("refresh") == "1" {
		maxAge = 0
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	byKey := map[string]*libraryItem{}
	var order []string
	var built time.Time
	var problems []string
	for i, srv := range cfg.PlexServers {
		if srv.Token == "" {
			continue
		}
		idx, err := ui.plex.index(ctx, srv, maxAge)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if built.IsZero() || idx.built.Before(built) {
			built = idx.built
		}
		for key, refs := range idx.items {
			for _, ref := range refs {
				it := byKey[key]
				if it == nil {
					it = &libraryItem{Type: ref.Type, TMDB: ref.TMDB, Title: ref.Title, Year: ref.Year}
					byKey[key] = it
					order = append(order, key)
				}
				if ref.AddedAt > it.Added {
					it.Added = ref.AddedAt
				}
				// Prefer the poster from a Mediux-agent library: that's the
				// copy your picks change.
				if ref.Thumb != "" && (it.Poster == "" || ref.Custom && !anyCustom(it.Places)) {
					it.Poster = plexImageURL(i, ref.Thumb, 200, 300)
				}
				it.Places = append(it.Places, libraryPlace{srv.Name, ref.Library, ref.Custom})
			}
		}
	}
	sort.Strings(order)
	items := make([]libraryItem, 0, len(order))
	keys := make([]string, 0, len(order))
	for _, key := range order {
		it := byKey[key]
		if p := ui.provider.picks.get(it.Type, it.TMDB); p != nil {
			it.Pick = p.parts()
		}
		it.Aura = ui.provider.aura.pin(it.Type, it.TMDB) != nil
		if n, ok := ui.availability.count(key); ok {
			it.Art = &n
		}
		items = append(items, *it)
		keys = append(keys, key)
	}
	// Mark the check as running before answering, so the page knows to
	// follow its progress.
	if due := ui.availability.begin(keys); len(due) > 0 {
		go ui.availability.run(context.Background(), ui.provider.mediux, due)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{
		"configured": true, "items": items, "built": built, "problems": problems,
		"availability": ui.availability.progress(),
	})
}

func anyCustom(places []libraryPlace) bool {
	for _, p := range places {
		if p.Custom {
			return true
		}
	}
	return false
}

// libraryArt answers GET /api/library/art: just the artwork counts, for
// updating the page while the background check runs.
func (ui *webUI) libraryArt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"counts": ui.availability.all(), "availability": ui.availability.progress()})
}

// ---- Mediux availability --------------------------------------------------

type availEntry struct {
	Count   int       `json:"n"`
	Checked time.Time `json:"t"`
}

type availProgress struct {
	Running bool      `json:"running"`
	Done    int       `json:"done"`
	Total   int       `json:"total"`
	LastRun time.Time `json:"last_run,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// availability remembers how much Mediux artwork each title has, checked in
// the background and saved in /config so it survives restarts.
type availability struct {
	path string

	mu     sync.Mutex
	counts map[string]availEntry // "movie:603"
	prog   availProgress
	dirty  bool
}

const availStale = 24 * time.Hour

func loadAvailability(dir string) *availability {
	a := &availability{path: filepath.Join(dir, "mediux-availability.json"), counts: map[string]availEntry{}}
	if raw, err := os.ReadFile(a.path); err == nil {
		_ = json.Unmarshal(raw, &a.counts)
	}
	return a
}

func (a *availability) count(key string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.counts[key]
	return e.Count, ok
}

func (a *availability) all() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.counts))
	for k, e := range a.counts {
		out[k] = e.Count
	}
	return out
}

func (a *availability) progress() availProgress {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prog
}

func (a *availability) set(key string, n int) {
	a.mu.Lock()
	a.counts[key] = availEntry{n, time.Now()}
	a.dirty = true
	a.prog.Done++
	a.mu.Unlock()
}

func (a *availability) save() {
	a.mu.Lock()
	if !a.dirty {
		a.mu.Unlock()
		return
	}
	raw, err := json.Marshal(a.counts)
	a.dirty = false
	a.mu.Unlock()
	if err != nil {
		return
	}
	tmp := a.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, a.path)
	}
}

const availBatch = 100

// begin picks the titles not checked in the last day and marks a check as
// running. It returns nothing when a check is already running or nothing is
// due. Only one check runs at a time.
func (a *availability) begin(keys []string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prog.Running {
		return nil
	}
	var due []string
	for _, k := range keys {
		if e, ok := a.counts[k]; !ok || time.Since(e.Checked) > availStale {
			due = append(due, k)
		}
	}
	if len(due) > 0 {
		a.prog = availProgress{Running: true, Total: len(due), LastRun: a.prog.LastRun}
	}
	return due
}

// run looks the due titles up, a hundred per Mediux query.
func (a *availability) run(ctx context.Context, m *mediuxClient, due []string) {
	byType := map[mediaType][]string{}
	for _, k := range due {
		typ, id, _ := strings.Cut(k, ":")
		byType[mediaType(typ)] = append(byType[mediaType(typ)], id)
	}
	var failure string
	for typ, ids := range byType {
		for start := 0; start < len(ids) && ctx.Err() == nil; start += availBatch {
			batch := ids[start:min(len(ids), start+availBatch)]
			counts, err := m.artworkCounts(ctx, typ, batch)
			if err != nil {
				failure = err.Error()
				log.Printf("mediux availability: %v", err)
				break
			}
			for _, id := range batch {
				a.set(string(typ)+":"+id, counts[id])
			}
			a.save()
		}
	}
	a.mu.Lock()
	a.prog.Running, a.prog.LastRun, a.prog.Error = false, time.Now(), failure
	a.mu.Unlock()
	a.save()
}

// artworkCounts asks Mediux how many images it has for many titles at once.
// Titles Mediux doesn't know count as 0.
func (c *mediuxClient) artworkCounts(ctx context.Context, typ mediaType, ids []string) (map[string]int, error) {
	collection := "movies"
	if typ == typeShow {
		collection = "shows"
	}
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		if numericID(id) { // IDs go into the query text, so only digits
			quoted = append(quoted, `"`+id+`"`)
		}
	}
	q := fmt.Sprintf(`{items:%s(limit:-1,filter:{id:{_in:[%s]}}){id posters_func{count}}}`, collection, strings.Join(quoted, ","))
	var out struct {
		Items []struct {
			ID      string `json:"id"`
			Posters struct {
				Count int `json:"count"`
			} `json:"posters_func"`
		} `json:"items"`
	}
	if err := c.query(ctx, collection+" artwork counts", q, nil, &out); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, it := range out.Items {
		counts[it.ID] = it.Posters.Count
	}
	return counts, nil
}
