package main

// AURA picks: AURA (github.com/mediux-team/AURA) stores the Mediux sets you
// saved per title in AURA.db. When that file is mounted (read-only), the
// provider uses those picks ahead of creator priority, so AURA stays the place
// to choose a specific set and every Plex server follows it.

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// auraPin holds the asset IDs AURA saved for one movie or show.
type auraPin struct {
	Type      string            `json:"type"` // movie or show
	TMDB      string            `json:"tmdb"`
	Title     string            `json:"title"`
	Year      int               `json:"year"`
	Libraries []string          `json:"libraries"` // AURA library titles, e.g. "Movies", "TV Anime"
	Poster    string            `json:"-"`
	Backdrop  string            `json:"-"`
	Seasons   map[int]string    `json:"-"`
	Cards     map[[2]int]string `json:"-"`
}

type auraStatus struct {
	Path     string    `json:"path"`
	Found    bool      `json:"found"`
	Movies   int       `json:"movies"`
	Shows    int       `json:"shows"`
	LoadedAt time.Time `json:"loaded_at,omitempty"`
	Error    string    `json:"error,omitempty"`
}

type auraStore struct {
	path func() string

	mu        sync.Mutex
	pins      map[string]*auraPin // "movie:603", "show:1413"
	loadedFor string              // path the pins came from
	modTime   time.Time
	checked   time.Time
	status    auraStatus
}

func newAuraStore(path func() string) *auraStore {
	return &auraStore{path: path, pins: map[string]*auraPin{}}
}

const auraCheckEvery = time.Minute

// refresh reloads AURA.db when it has changed, at most once a minute.
func (a *auraStore) refresh(force bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path := strings.TrimSpace(a.path())
	if !force && path == a.loadedFor && time.Since(a.checked) < auraCheckEvery {
		return
	}
	a.checked = time.Now()
	a.status.Path = path
	if path == "" {
		a.pins, a.loadedFor, a.modTime = map[string]*auraPin{}, "", time.Time{}
		a.status = auraStatus{}
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		a.pins, a.loadedFor, a.modTime = map[string]*auraPin{}, path, time.Time{}
		a.status = auraStatus{Path: path}
		return
	}
	if !force && path == a.loadedFor && info.ModTime().Equal(a.modTime) {
		return
	}
	pins, err := readAuraDB(path)
	a.loadedFor, a.modTime = path, info.ModTime()
	if err != nil {
		// Keep the previous picks if a read fails midway through an AURA save.
		a.status.Found, a.status.Error = true, err.Error()
		return
	}
	a.pins = pins
	a.status = auraStatus{Path: path, Found: true, LoadedAt: time.Now()}
	for _, p := range pins {
		if p.Type == "movie" {
			a.status.Movies++
		} else {
			a.status.Shows++
		}
	}
}

func (a *auraStore) pin(typ mediaType, tmdb string) *auraPin {
	if a == nil {
		return nil
	}
	a.refresh(false)
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pins[string(typ)+":"+tmdb]
}

func (a *auraStore) snapshot() (auraStatus, []auraPin) {
	if a == nil {
		return auraStatus{}, nil
	}
	a.refresh(false)
	a.mu.Lock()
	defer a.mu.Unlock()
	list := make([]auraPin, 0, len(a.pins))
	for _, p := range a.pins {
		list = append(list, *p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Type != list[j].Type {
			return list[i].Type < list[j].Type
		}
		return strings.ToLower(list[i].Title) < strings.ToLower(list[j].Title)
	})
	return a.status, list
}

// parts describes what a pin covers, for the settings page.
func (p auraPin) parts() string {
	var out []string
	if p.Poster != "" {
		out = append(out, "poster")
	}
	if p.Backdrop != "" {
		out = append(out, "background")
	}
	if n := len(p.Seasons); n > 0 {
		out = append(out, plural(n, "season poster", "season posters"))
	}
	if n := len(p.Cards); n > 0 {
		out = append(out, plural(n, "title card", "title cards"))
	}
	return strings.Join(out, ", ")
}

// readAuraDB reads AURA's saved selections. Later saves win where two saved
// sets cover the same image, matching AURA's own behavior.
func readAuraDB(path string) (map[string]*auraPin, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	rows, err := db.Query(`
		SELECT si.tmdb_id, si.poster_set_id, COALESCE(si.library_title, ''),
		       COALESCE(si.poster_selected, 0), COALESCE(si.backdrop_selected, 0),
		       COALESCE(si.season_poster_selected, 0), COALESCE(si.special_season_poster_selected, 0),
		       COALESCE(si.titlecard_selected, 0),
		       COALESCE(mi.type, ''), COALESCE(mi.title, ''), COALESCE(mi.year, 0)
		FROM SavedItems si
		JOIN MediaItems mi ON mi.tmdb_id = si.tmdb_id AND mi.library_title = si.library_title
		ORDER BY si.last_downloaded`)
	if err != nil {
		return nil, fmt.Errorf("reading AURA saved items: %w", err)
	}
	type saved struct {
		tmdb, set, library                         string
		poster, backdrop, seasons, specials, cards bool
		typ, title                                 string
		year                                       int
	}
	var all []saved
	for rows.Next() {
		var s saved
		var tmdb, set any
		if err := rows.Scan(&tmdb, &set, &s.library, &s.poster, &s.backdrop, &s.seasons, &s.specials, &s.cards, &s.typ, &s.title, &s.year); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading AURA saved items: %w", err)
		}
		s.tmdb, s.set = fmt.Sprint(tmdb), fmt.Sprint(set)
		all = append(all, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	pins := map[string]*auraPin{}
	for _, s := range all {
		typ := "show"
		if s.typ == "movie" {
			typ = "movie"
		}
		key := typ + ":" + s.tmdb
		p := pins[key]
		if p == nil {
			p = &auraPin{Type: typ, TMDB: s.tmdb, Title: s.title, Year: s.year,
				Seasons: map[int]string{}, Cards: map[[2]int]string{}}
			pins[key] = p
		}
		if s.library != "" && !containsString(p.Libraries, s.library) {
			p.Libraries = append(p.Libraries, s.library)
		}
		images, err := db.Query(`
			SELECT image_id, COALESCE(image_type, ''), image_season_number, image_episode_number
			FROM ImageFiles WHERE poster_set_id = ? AND item_tmdb_id = ?`, s.set, s.tmdb)
		if err != nil {
			return nil, fmt.Errorf("reading AURA images: %w", err)
		}
		for images.Next() {
			var id, kind string
			var season, episode sql.NullInt64
			if err := images.Scan(&id, &kind, &season, &episode); err != nil {
				images.Close()
				return nil, fmt.Errorf("reading AURA images: %w", err)
			}
			if !assetID.MatchString(id) {
				continue
			}
			switch kind {
			case "poster":
				if s.poster {
					p.Poster = id
				}
			case "backdrop":
				if s.backdrop {
					p.Backdrop = id
				}
			case "season_poster":
				if season.Valid && ((season.Int64 == 0 && s.specials) || (season.Int64 > 0 && s.seasons)) {
					p.Seasons[int(season.Int64)] = id
				}
			case "titlecard":
				if s.cards && season.Valid && episode.Valid {
					p.Cards[[2]int{int(season.Int64), int(episode.Int64)}] = id
				}
			}
		}
		images.Close()
	}
	for k, p := range pins {
		if p.Poster == "" && p.Backdrop == "" && len(p.Seasons) == 0 && len(p.Cards) == 0 {
			delete(pins, k)
		}
	}
	return pins, nil
}

// pinned finds a pinned asset in the current Mediux artwork, so its version
// stamp and creator are known. Creator exclusions don't apply to AURA picks.
func pinned(b bundle, id string) (artwork, bool) {
	if id == "" {
		return artwork{}, false
	}
	for _, s := range b.Sets {
		for _, a := range s.Art {
			if a.ID == id {
				return a, true
			}
		}
	}
	return artwork{}, false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
