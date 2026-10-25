package main

// Watching for new artwork. Plex only asks the provider about an item when it
// is added or refreshed, so a title card a creator adds a week after the
// episode aired would otherwise never arrive. The watcher remembers what each
// title Plex asked about currently gets, re-checks Mediux on a schedule, and
// when something new appears it asks Plex to refresh that item and posts to
// Discord.
//
// Changes caused by your own settings (creator order, consistency rules, AURA
// picks) are recorded silently: those apply on the next refresh as usual and
// must not trigger a flood of refreshes or messages.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type trackedItem struct {
	Type      mediaType         `json:"type"`
	TMDB      string            `json:"tmdb"`
	Title     string            `json:"title"`
	Language  string            `json:"language"`
	Signature map[string]string `json:"signature"` // slot -> asset@version
	Basis     string            `json:"basis"`     // settings + AURA pick the signature was made under
	Checked   time.Time         `json:"checked"`
	Added     time.Time         `json:"added"`
}

type watchStatus struct {
	Tracked   int       `json:"tracked"`
	LastRun   time.Time `json:"last_run,omitempty"`
	LastFound int       `json:"last_found"`
	Running   bool      `json:"running"`
	Active    bool      `json:"active"`
	Reason    string    `json:"reason,omitempty"`
}

type watcher struct {
	p       *plexProvider
	plex    *plexClient
	discord *discord
	path    string

	mu      sync.Mutex
	items   map[string]*trackedItem
	dirty   bool
	status  watchStatus
	running bool
}

func newWatcher(p *plexProvider, plex *plexClient, d *discord, dir string) *watcher {
	w := &watcher{p: p, plex: plex, discord: d, path: filepath.Join(dir, "tracked.json"), items: map[string]*trackedItem{}}
	if raw, err := os.ReadFile(w.path); err == nil {
		if err := json.Unmarshal(raw, &w.items); err != nil {
			log.Printf("watch: ignoring unreadable %s: %v", w.path, err)
			w.items = map[string]*trackedItem{}
		}
	}
	return w
}

// basis fingerprints everything on our side that decides which art is picked.
func basis(cfg settings, o override) string {
	h := sha256.New()
	fmt.Fprintf(h, "%q|%q|%v|%s|%s|%s|%s|", cfg.PriorityCreators, cfg.ExcludedCreators, cfg.OnlyPriority,
		cfg.SeasonConsistency, cfg.TitlecardConsistency, cfg.MissingArtwork, cfg.ApplyMode)
	if pin := o.aura; pin != nil {
		fmt.Fprintf(h, "%s|%s|%v|%v|", pin.Poster, pin.Backdrop, pin.Seasons, pin.Cards)
	}
	if p := o.pick; p != nil {
		fmt.Fprintf(h, "%s|%s|%s|%s|%v", p.PosterSet, p.BackSet, p.SeasonSet, p.CardSet, p.KeepPlex)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func slot(c choice) string {
	return c.art.ID + "@" + c.art.Modified.UTC().Format("20060102150405")
}

func movieSignature(b bundle, cfg settings, language string, o override) map[string]string {
	sig := map[string]string{}
	poster := choosePoster(b, cfg, language, o)
	if poster.ok {
		sig["poster"] = slot(poster)
	}
	if c := chooseWithPoster(b, cfg, kindBackdrop, language, poster, o); c.ok {
		sig["background"] = slot(c)
	}
	if c := chooseWithPoster(b, cfg, kindLogo, language, poster, o); c.ok {
		sig["logo"] = slot(c)
	}
	return sig
}

func (sh *show) signature() map[string]string {
	sig := map[string]string{}
	if c := sh.poster(); c.ok {
		sig["poster"] = slot(c)
	}
	if c := sh.backdrop(); c.ok {
		sig["background"] = slot(c)
	}
	if c := sh.logo(); c.ok {
		sig["logo"] = slot(c)
	}
	for s, eps := range sh.structure {
		if c := sh.season(s); c.ok {
			sig[fmt.Sprintf("s%d", s)] = slot(c)
		}
		for _, e := range eps {
			if c := sh.card(s, e); c.ok {
				sig[fmt.Sprintf("s%de%d", s, e)] = slot(c)
			}
		}
	}
	return sig
}

// observe records what Plex was just given for a title. A title seen for the
// first time with artwork can be announced on Discord.
func (w *watcher) observe(typ mediaType, tmdb, title, language string, sig map[string]string, base string, posterURL, summary string) {
	if w == nil {
		return
	}
	key := string(typ) + ":" + tmdb
	w.mu.Lock()
	it := w.items[key]
	isNew := it == nil
	if isNew {
		it = &trackedItem{Type: typ, TMDB: tmdb, Added: time.Now(), Checked: time.Now()}
		w.items[key] = it
	}
	if title != "" && !strings.HasPrefix(title, "TMDB ") {
		it.Title = title
	} else if it.Title == "" {
		it.Title = title
	}
	if language != "" {
		it.Language = language
	}
	// Plex now has this art, so it is the new baseline either way.
	it.Signature, it.Basis = sig, base
	w.dirty = true
	w.mu.Unlock()

	if isNew && len(sig) > 0 && w.p.settings.get().NotifyNewItems {
		w.discord.add(discordEmbed{Title: it.Title, Description: "Mediux artwork added: " + summary,
			Color: colorNew, Thumbnail: thumb(posterURL), Footer: &discordFooter{Text: "New in Plex"}})
	}
}

func thumb(u string) *discordImage {
	if u == "" {
		return nil
	}
	return &discordImage{URL: u}
}

// run checks due titles every ten minutes and saves the list every minute.
func (w *watcher) run(ctx context.Context) {
	save := time.NewTicker(time.Minute)
	check := time.NewTicker(10 * time.Minute)
	defer save.Stop()
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			w.save()
			return
		case <-save.C:
			w.save()
		case <-check.C:
			w.checkDue(ctx, false)
		}
	}
}

func (w *watcher) save() {
	w.mu.Lock()
	if !w.dirty {
		w.mu.Unlock()
		return
	}
	raw, err := json.Marshal(w.items)
	w.dirty = false
	w.mu.Unlock()
	if err != nil {
		return
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		_ = os.Rename(tmp, w.path)
	}
}

// active reports whether watching would do anything useful right now.
func (w *watcher) active(cfg settings) (bool, string) {
	if cfg.WatchHours <= 0 {
		return false, "Watching is turned off."
	}
	canRefresh := false
	for _, s := range cfg.PlexServers {
		if s.Token != "" {
			canRefresh = true
		}
	}
	if !(cfg.RefreshPlex && canRefresh) && cfg.DiscordWebhook == "" {
		return false, "Add a Plex server or a Discord webhook so new artwork has somewhere to go."
	}
	return true, ""
}

const watchBatch = 150 // titles checked per ten-minute round, to go easy on Mediux

// checkDue re-checks titles not checked within the watch interval. With
// all=true (the "Check now" button) every title is due.
func (w *watcher) checkDue(ctx context.Context, all bool) int {
	cfg := w.p.settings.get()
	if ok, _ := w.active(cfg); !ok {
		return 0
	}
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return 0
	}
	w.running = true
	cutoff := time.Now().Add(-time.Duration(cfg.WatchHours) * time.Hour)
	var due []trackedItem
	for _, it := range w.items {
		if all || it.Checked.Before(cutoff) {
			due = append(due, *it)
		}
	}
	w.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].Checked.Before(due[j].Checked) })
	if !all && len(due) > watchBatch {
		due = due[:watchBatch]
	}

	found := 0
	for _, it := range due {
		if ctx.Err() != nil {
			break
		}
		if w.checkOne(ctx, it, cfg) {
			found++
		}
	}
	w.mu.Lock()
	w.running = false
	w.status.LastRun, w.status.LastFound = time.Now(), found
	w.mu.Unlock()
	return found
}

// checkOne re-reads Mediux for one title and acts on anything new.
func (w *watcher) checkOne(ctx context.Context, it trackedItem, cfg settings) bool {
	key := string(it.Type) + ":" + it.TMDB
	w.p.mediux.forget(it.Type, it.TMDB)
	pr := &plexRequest{language: it.Language}
	var sig map[string]string
	var ov override
	var posterURL string
	if it.Type == typeMovie {
		b, err := w.p.mediux.artwork(ctx, typeMovie, it.TMDB)
		if err != nil {
			w.markChecked(key)
			return false
		}
		ov = w.p.overrides(typeMovie, it.TMDB)
		if w.p.gate(typeMovie, it.TMDB) != "" {
			b = bundle{} // not approved: nothing new for Plex to get
		}
		sig = movieSignature(b, cfg, it.Language, ov)
		if c := choosePoster(b, cfg, it.Language, ov); c.ok {
			posterURL = imageURL(c.art, "optimized")
		}
	} else {
		sh, err := w.p.loadShow(ctx, pr, it.TMDB)
		if err == nil {
			sh.block(w.p.gate(typeShow, it.TMDB))
		}
		if err != nil {
			w.markChecked(key)
			return false
		}
		ov, sig = sh.ov, sh.signature()
		if c := sh.poster(); c.ok {
			posterURL = imageURL(c.art, "optimized")
		}
	}
	base := basis(cfg, ov)

	w.mu.Lock()
	cur := w.items[key]
	if cur == nil {
		w.mu.Unlock()
		return false
	}
	changed := []string{}
	if cur.Basis == base {
		changed = newSlots(cur.Signature, sig)
	}
	cur.Signature, cur.Basis, cur.Checked = sig, base, time.Now()
	w.dirty = true
	w.mu.Unlock()
	if len(changed) == 0 {
		return false
	}

	what := describeSlots(changed)
	footer := "Plex refresh is off"
	if cfg.RefreshPlex {
		res, err := w.plex.refresh(ctx, cfg.PlexServers, it.Type, it.TMDB, false)
		footer = res.describe()
		if err != nil {
			footer += "; Plex refresh problem: " + err.Error()
		}
	}
	w.p.activity.add(string(it.Type), it.Title, "new artwork", what+"; "+footer)
	if cfg.NotifyUpdates {
		w.discord.add(discordEmbed{Title: it.Title, Description: "New Mediux artwork: " + what,
			Color: colorUpdated, Thumbnail: thumb(posterURL), Footer: &discordFooter{Text: footer}})
	}
	return true
}

func (w *watcher) markChecked(key string) {
	w.mu.Lock()
	if it := w.items[key]; it != nil {
		it.Checked = time.Now()
		w.dirty = true
	}
	w.mu.Unlock()
}

func (w *watcher) snapshot() watchStatus {
	cfg := w.p.settings.get()
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.status
	st.Tracked, st.Running = len(w.items), w.running
	st.Active, st.Reason = w.active(cfg)
	return st
}

// newSlots lists slots that are new or point at different artwork.
func newSlots(old, cur map[string]string) []string {
	var out []string
	for k, v := range cur {
		if old[k] != v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// describeSlots turns ["poster","s2","s2e5","s2e6"] into readable text.
func describeSlots(slots []string) string {
	var parts, seasons, cards []string
	for _, s := range slots {
		switch {
		case s == "poster" || s == "background" || s == "logo":
			parts = append(parts, s)
		case strings.Contains(s, "e"):
			var sn, en int
			if _, err := fmt.Sscanf(s, "s%de%d", &sn, &en); err == nil {
				cards = append(cards, fmt.Sprintf("S%dE%d", sn, en))
			}
		default:
			seasons = append(seasons, strings.TrimPrefix(s, "s"))
		}
	}
	if len(seasons) > 0 {
		parts = append(parts, "season posters "+strings.Join(seasons, ", "))
	}
	if len(cards) > 0 {
		list := cards
		if len(list) > 8 {
			list = append(list[:8:8], fmt.Sprintf("+%d more", len(cards)-8))
		}
		parts = append(parts, "title cards "+strings.Join(list, ", "))
	}
	return strings.Join(parts, "; ")
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

var errWatchBusy = errors.New("a check is already running")
