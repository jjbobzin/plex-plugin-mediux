package main

// The Sets tab: every Mediux set for a title, side by side, and saving the
// set you want for each part of the artwork.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type setImage struct {
	Season  int    `json:"season,omitempty"`
	Episode int    `json:"episode,omitempty"`
	URL     string `json:"url"`
}

type setCard struct {
	ID         string     `json:"id"`
	Creator    string     `json:"creator"`
	Popularity float64    `json:"popularity"`
	Favorite   int        `json:"favorite,omitempty"` // 1-based rank in creator priority
	Excluded   bool       `json:"excluded,omitempty"`
	Poster     string     `json:"poster,omitempty"`
	Background string     `json:"background,omitempty"`
	Logo       string     `json:"logo,omitempty"`
	Seasons    []setImage `json:"seasons"`
	Cards      []setImage `json:"cards"`
	CardCount  int        `json:"card_count"`
	Collection string     `json:"collection,omitempty"` // name, for movie collection sets
}

const cardsPerSet = 60 // thumbnails sent to the page per set; the count is exact

func thumbURL(a artwork) string { return imageURL(a, "optimized") }

// setsFor describes every set for a title, favorites first.
func setsFor(b bundle, cfg settings) []setCard {
	priority, excluded := rankMap(cfg.PriorityCreators), rankMap(cfg.ExcludedCreators)
	out := []setCard{}
	for _, s := range b.Sets {
		c := setCard{ID: s.ID, Creator: s.Creator, Popularity: s.Popularity, Seasons: []setImage{}, Cards: []setImage{}, Collection: s.Collection}
		if r, ok := priority[strings.ToLower(s.Creator)]; ok {
			c.Favorite = r + 1
		}
		_, c.Excluded = excluded[strings.ToLower(s.Creator)]
		for _, a := range s.Art {
			switch a.Kind {
			case kindPoster:
				if c.Poster == "" || a.Language == "en" {
					c.Poster = thumbURL(a)
				}
			case kindBackdrop:
				if c.Background == "" || a.Language == "" {
					c.Background = thumbURL(a)
				}
			case kindLogo:
				if c.Logo == "" {
					c.Logo = thumbURL(a)
				}
			case kindSeason:
				c.Seasons = append(c.Seasons, setImage{Season: a.Season, URL: thumbURL(a)})
			case kindStill:
				c.CardCount++
				c.Cards = append(c.Cards, setImage{Season: a.Season, Episode: a.Episode, URL: thumbURL(a)})
			}
		}
		sort.Slice(c.Seasons, func(i, j int) bool { return c.Seasons[i].Season < c.Seasons[j].Season })
		c.Seasons = dedupeImages(c.Seasons)
		sort.Slice(c.Cards, func(i, j int) bool {
			if c.Cards[i].Season != c.Cards[j].Season {
				return c.Cards[i].Season < c.Cards[j].Season
			}
			return c.Cards[i].Episode < c.Cards[j].Episode
		})
		c.Cards = dedupeImages(c.Cards)
		c.CardCount = len(c.Cards)
		if len(c.Cards) > cardsPerSet {
			c.Cards = c.Cards[:cardsPerSet]
		}
		if c.Poster == "" && c.Background == "" && c.Logo == "" && len(c.Seasons) == 0 && c.CardCount == 0 {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		fi, fj := out[i].Favorite, out[j].Favorite
		if (fi > 0) != (fj > 0) {
			return fi > 0
		}
		if fi != fj {
			return fi < fj
		}
		if out[i].Excluded != out[j].Excluded {
			return !out[i].Excluded
		}
		return out[i].Popularity > out[j].Popularity
	})
	return out
}

// dedupeImages keeps one image per season/episode (sets can hold language variants).
func dedupeImages(in []setImage) []setImage {
	out := in[:0]
	seen := map[[2]int]bool{}
	for _, x := range in {
		k := [2]int{x.Season, x.Episode}
		if !seen[k] {
			seen[k] = true
			out = append(out, x)
		}
	}
	return out
}

type pickParts struct {
	Poster     string `json:"poster_set"`
	Background string `json:"background_set"`
	Season     string `json:"season_set"`
	Card       string `json:"titlecard_set"`
}

// sets answers the Sets tab: GET /api/sets?type=show&q=1923
func (ui *webUI) sets(w http.ResponseWriter, r *http.Request) {
	kind := mediaType(r.URL.Query().Get("type"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tmdb, how, status, msg := ui.resolve(ctx, string(kind), strings.TrimSpace(r.URL.Query().Get("q")))
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	b, err := ui.provider.mediux.artwork(ctx, kind, tmdb)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	cfg := ui.settings.get()
	title, year := ui.provider.mediux.titleOf(ctx, kind, tmdb)
	// What Plex gets today without a page pick: AURA's pick if there is one,
	// else the rules.
	cur := override{aura: ui.provider.aura.pin(kind, tmdb)}
	auto := pickParts{}
	poster := choosePoster(b, cfg, "en", cur)
	if poster.ok {
		auto.Poster = poster.art.SetID
	}
	if c := chooseWithPoster(b, cfg, kindBackdrop, "en", poster, cur); c.ok {
		auto.Background = c.art.SetID
	}
	if kind == typeShow {
		auto.Season = chooseSet(b, cfg, kindSeason, "en").ID
		auto.Card = chooseSet(b, cfg, kindStill, "en").ID
		if pin := cur.aura; pin != nil {
			if id := mostCommonSet(b, mapValues(pin.Seasons)); id != "" {
				auto.Season = id
			}
			if id := mostCommonSet(b, mapValues(pin.Cards)); id != "" {
				auto.Card = id
			}
		}
	}
	var picked *pickParts
	if p := ui.provider.picks.get(kind, tmdb); p != nil {
		picked = &pickParts{p.PosterSet, p.BackSet, p.SeasonSet, p.CardSet}
	}
	writeJSON(w, 200, map[string]any{
		"type": kind, "tmdb": tmdb, "how": how, "title": title, "year": year,
		"sets": setsFor(b, cfg), "auto": auto, "pick": picked,
		"aura":   ui.provider.aura.pin(kind, tmdb) != nil,
		"others": ui.otherMatches(ctx, kind, r.URL.Query().Get("q"), tmdb),
	})
}

func (ui *webUI) listPicks(w http.ResponseWriter, _ *http.Request) {
	type row struct {
		userPick
		Parts string `json:"parts"`
	}
	rows := []row{}
	for _, p := range ui.provider.picks.list() {
		rows = append(rows, row{p, p.parts()})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"items": rows})
}

// savePick stores the chosen sets for a title, then asks Plex to refresh it.
func (ui *webUI) savePick(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type  mediaType `json:"type"`
		TMDB  string    `json:"tmdb"`
		Title string    `json:"title"`
		Year  int       `json:"year"`
		pickParts
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "could not read the pick"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if in.Type != typeMovie && in.Type != typeShow || !numericID(in.TMDB) {
		writeJSON(w, 400, map[string]string{"error": "unknown title"})
		return
	}
	b, err := ui.provider.mediux.artwork(ctx, in.Type, in.TMDB)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	known := map[string]bool{"": true}
	for _, s := range b.Sets {
		known[s.ID] = true
	}
	for _, id := range []string{in.Poster, in.Background, in.Season, in.Card} {
		if !known[id] {
			writeJSON(w, 400, map[string]string{"error": "that set is no longer on Mediux; look the title up again"})
			return
		}
	}
	p := userPick{Type: in.Type, TMDB: in.TMDB, Title: strings.TrimSpace(in.Title), Year: in.Year,
		PosterSet: in.Poster, BackSet: in.Background, SeasonSet: in.Season, CardSet: in.Card}
	if len(p.Title) > 200 {
		p.Title = p.Title[:200]
	}
	if err := ui.provider.picks.put(p); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	label := p.Title
	if label == "" {
		label = "TMDB " + p.TMDB
	}
	what := "Pick saved"
	if p.empty() {
		what = "Pick removed"
	}
	cfg := ui.settings.get()
	if !cfg.RefreshPlex || !hasPlexToken(cfg.PlexServers) {
		ui.activity.add(string(p.Type), label, "picked", what+"; refresh the title in Plex to see it")
		writeJSON(w, 200, map[string]string{"status": what + ". Refresh the title in Plex to see it, or add your Plex servers on Watch & notify to do that automatically."})
		return
	}
	// Refresh in the background: unlocking a long show can take a while.
	go func() {
		bg, done := context.WithTimeout(context.Background(), 3*time.Minute)
		defer done()
		res, err := ui.plex.refresh(bg, cfg.PlexServers, p.Type, p.TMDB, cfg.ReplaceLocked)
		detail := what + "; " + res.describe()
		result := "picked"
		if err != nil {
			detail += "; Plex refresh problem: " + err.Error()
			result = "error"
			log.Printf("pick refresh: %v", err)
		}
		ui.activity.add(string(p.Type), label, result, detail)
	}()
	note := ""
	if cfg.ReplaceLocked {
		note = " Locked artwork will be replaced."
	}
	writeJSON(w, 200, map[string]string{"status": fmt.Sprintf("%s. Asking Plex to refresh %s now.%s The Log shows the result.", what, label, note)})
}

func hasPlexToken(servers []plexServer) bool {
	for _, s := range servers {
		if s.Token != "" {
			return true
		}
	}
	return false
}

// mostCommonSet names the set most of these asset IDs come from. AURA keeps
// season posters and title cards image by image, so this is the set "in use".
func mostCommonSet(b bundle, ids []string) string {
	count := map[string]int{}
	best, most := "", 0
	for _, id := range ids {
		if a, ok := pinned(b, id); ok {
			count[a.SetID]++
			if count[a.SetID] > most || count[a.SetID] == most && a.SetID < best {
				best, most = a.SetID, count[a.SetID]
			}
		}
	}
	return best
}

func mapValues[K comparable](m map[K]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

type titleChoice struct {
	TMDB  string `json:"tmdb"`
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
}

// otherMatches lists other Mediux titles for what was typed, so "Evil Dead"
// can lead to The Evil Dead (1981) as well as the 2013 film.
func (ui *webUI) otherMatches(ctx context.Context, kind mediaType, typed, chosen string) []titleChoice {
	out := []titleChoice{}
	typed = strings.TrimSpace(typed)
	if typed == "" || numericID(typed) {
		return out
	}
	title, _ := splitYear(typed, 0)
	var list []titleMatch
	var err error
	if kind == typeMovie {
		list, err = ui.provider.mediux.searchMovies(ctx, title)
	} else {
		list, err = ui.provider.mediux.searchShows(ctx, title)
	}
	if err != nil {
		return out
	}
	for _, m := range list {
		if m.ID != chosen && len(out) < 10 {
			out = append(out, titleChoice{m.ID, m.Title, yearOf(m.Date)})
		}
	}
	return out
}

// plexNow shows what each Plex server displays for a title right now.
func (ui *webUI) plexNow(w http.ResponseWriter, r *http.Request) {
	kind := mediaType(r.URL.Query().Get("type"))
	tmdb := r.URL.Query().Get("tmdb")
	if kind != typeMovie && kind != typeShow || !numericID(tmdb) {
		writeJSON(w, 400, map[string]string{"error": "unknown title"})
		return
	}
	cfg := ui.settings.get()
	if !hasPlexToken(cfg.PlexServers) {
		writeJSON(w, 200, map[string]any{"configured": false, "servers": []any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cur := ui.plex.current(ctx, cfg.PlexServers, kind, tmdb)
	if cur == nil {
		cur = []plexCurrent{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"configured": true, "servers": cur})
}

// plexImage relays an image from a Plex server, adding its token.
func (ui *webUI) plexImage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i, err := strconv.Atoi(q.Get("s"))
	servers := ui.settings.get().PlexServers
	if err != nil || i < 0 || i >= len(servers) || servers[i].Token == "" {
		http.NotFound(w, r)
		return
	}
	width, _ := strconv.Atoi(q.Get("w"))
	height, _ := strconv.Atoi(q.Get("h"))
	width, height = min(max(width, 40), 1200), min(max(height, 40), 1800)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := ui.plex.image(ctx, servers[i], q.Get("p"), width, height)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer res.Body.Close()
	ct := res.Header.Get("Content-Type")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") {
		http.Error(w, "Plex returned "+res.Status, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = io.Copy(w, io.LimitReader(res.Body, 20<<20))
}

// collection lists the movies a collection set covers.
func (ui *webUI) collection(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !numericID(id) {
		writeJSON(w, 400, map[string]string{"error": "unknown collection set"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	movies, err := ui.provider.mediux.collectionMovies(ctx, id)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"movies": movies})
}

// saveCollectionPick picks one collection set for every movie it covers.
func (ui *webUI) saveCollectionPick(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SetID      string `json:"set_id"`
		Poster     bool   `json:"poster"`
		Background bool   `json:"background"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil || !numericID(in.SetID) || !(in.Poster || in.Background) {
		writeJSON(w, 400, map[string]string{"error": "choose a collection set and what to use it for"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	movies, err := ui.provider.mediux.collectionMovies(ctx, in.SetID)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	var picked []userPick
	for _, m := range movies {
		p := userPick{Type: typeMovie, TMDB: m.TMDB, Title: m.Title, Year: m.Year}
		if old := ui.provider.picks.get(typeMovie, m.TMDB); old != nil {
			p = *old
		}
		changed := false
		if in.Poster && m.Poster {
			p.PosterSet, changed = in.SetID, true
		}
		if in.Background && m.Background {
			p.BackSet, changed = in.SetID, true
		}
		if !changed {
			continue
		}
		if err := ui.provider.picks.put(p); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		picked = append(picked, p)
	}
	if len(picked) == 0 {
		writeJSON(w, 400, map[string]string{"error": "this collection set has no artwork of that kind"})
		return
	}
	names := make([]string, 0, len(picked))
	for _, p := range picked {
		names = append(names, p.Title)
	}
	cfg := ui.settings.get()
	status := fmt.Sprintf("Saved for %s: %s.", plural(len(picked), "movie", "movies"), strings.Join(names, ", "))
	if !cfg.RefreshPlex || !hasPlexToken(cfg.PlexServers) {
		writeJSON(w, 200, map[string]string{"status": status + " Refresh them in Plex to see the change."})
		return
	}
	go func() {
		for _, p := range picked {
			bg, done := context.WithTimeout(context.Background(), 3*time.Minute)
			res, err := ui.plex.refresh(bg, cfg.PlexServers, typeMovie, p.TMDB, cfg.ReplaceLocked)
			done()
			detail, result := "Collection pick saved; "+res.describe(), "picked"
			if err != nil {
				detail, result = detail+"; Plex refresh problem: "+err.Error(), "error"
			} else if len(res.Asked) == 0 && len(res.Skipped) == 0 {
				detail = "Collection pick saved; not in your Plex libraries yet (it will be used when it's added)"
			}
			ui.activity.add("movie", p.Title, result, detail)
		}
	}()
	writeJSON(w, 200, map[string]string{"status": status + " Refreshing them in Plex now; the Log shows each one."})
}
