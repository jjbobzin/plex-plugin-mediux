package main

// Plex sends additional providers only a title, year and file name, never an
// external ID. This works out the TMDB ID that Mediux uses:
//  1. an ID tag in the file name, e.g. {tmdb-291870} or {tvdb-416491}
//  2. a title (+ year) lookup: TVmaze for shows, then Mediux itself

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

var (
	tmdbTag = regexp.MustCompile(`(?i)\btmdb(?:id)?[-_=: ]+(\d+)`)
	tvdbTag = regexp.MustCompile(`(?i)\btvdb(?:id)?[-_=: ]+(\d+)`)
	guidRe  = regexp.MustCompile(`^(tmdb|tvdb|imdb)://(.+)$`)
	yearRe  = regexp.MustCompile(`\s*\((\d{4})\)\s*$`)
)

type hints struct {
	Type             any    `json:"type"`
	Title            string `json:"title"`
	ParentTitle      string `json:"parentTitle"`
	GrandparentTitle string `json:"grandparentTitle"`
	Year             int    `json:"year"`
	Guid             string `json:"guid"`
	Index            *int   `json:"index"`
	ParentIndex      *int   `json:"parentIndex"`
	Filename         string `json:"filename"`
	IncludeChildren  any    `json:"includeChildren"`
	Manual           any    `json:"manual"`
}

func isOn(v any) bool {
	switch x := v.(type) {
	case float64:
		return x == 1
	case string:
		return x == "1" || x == "true"
	case bool:
		return x
	}
	return false
}

func typeNumber(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		switch x {
		case "movie":
			return 1
		case "show":
			return 2
		case "season":
			return 3
		case "episode":
			return 4
		}
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

// tagIDs pulls IDs out of a guid hint and the file name.
func tagIDs(h hints) (tmdb, tvdb string) {
	if m := guidRe.FindStringSubmatch(h.Guid); m != nil {
		switch m[1] {
		case "tmdb":
			tmdb = m[2]
		case "tvdb":
			tvdb = m[2]
		}
	}
	if m := tmdbTag.FindStringSubmatch(h.Filename); m != nil && tmdb == "" {
		tmdb = m[1]
	}
	if m := tvdbTag.FindStringSubmatch(h.Filename); m != nil && tvdb == "" {
		tvdb = m[1]
	}
	return tmdb, tvdb
}

// normTitle makes titles comparable: lower case, letters and digits only.
func normTitle(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "&", "and")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// splitYear turns "Show (2022)" into "Show", 2022.
func splitYear(title string, year int) (string, int) {
	if m := yearRe.FindStringSubmatch(title); m != nil {
		y, _ := strconv.Atoi(m[1])
		if year == 0 {
			year = y
		}
		title = strings.TrimSpace(yearRe.ReplaceAllString(title, ""))
	}
	return title, year
}

var fileYearRe = regexp.MustCompile(`\((\d{4})\)`)

// fileYear finds "(2022)" in the file name itself, ignoring folders.
func fileYear(filename string) int {
	base := filename[strings.LastIndexAny(filename, `/\`)+1:]
	if m := fileYearRe.FindStringSubmatch(base); m != nil {
		y, _ := strconv.Atoi(m[1])
		if y >= 1900 && y <= 2100 {
			return y
		}
	}
	return 0
}

func yearOf(date string) int {
	if len(date) >= 4 {
		y, _ := strconv.Atoi(date[:4])
		return y
	}
	return 0
}

// pickTitle returns the candidate whose title matches exactly, preferring the
// right year. Without an exact title match it returns nothing, so we never
// put the wrong artwork on an item.
func pickTitle(list []titleMatch, title string, year int) string {
	want := normTitle(title)
	var fallback string
	for _, m := range list {
		if normTitle(m.Title) != want {
			continue
		}
		y := yearOf(m.Date)
		if year == 0 || y == year {
			return m.ID
		}
		if fallback == "" && (y == year-1 || y == year+1) {
			fallback = m.ID
		}
	}
	return fallback
}

type idCacheEntry struct {
	id    string
	until time.Time
}

type identifier struct {
	mediux *mediuxClient
	http   *http.Client
	tvmaze string

	mu    sync.Mutex
	cache map[string]idCacheEntry
}

func newIdentifier(m *mediuxClient) *identifier {
	return &identifier{mediux: m, http: &http.Client{Timeout: 10 * time.Second},
		tvmaze: "https://api.tvmaze.com", cache: map[string]idCacheEntry{}}
}

func (id *identifier) cached(key string) (string, bool) {
	id.mu.Lock()
	defer id.mu.Unlock()
	e, ok := id.cache[key]
	if !ok || time.Now().After(e.until) {
		return "", false
	}
	return e.id, true
}

func (id *identifier) remember(key, value string) {
	ttl := 24 * time.Hour
	if value == "" {
		ttl = 30 * time.Minute
	}
	id.mu.Lock()
	if len(id.cache) > 20000 {
		id.cache = map[string]idCacheEntry{}
	}
	id.cache[key] = idCacheEntry{value, time.Now().Add(ttl)}
	id.mu.Unlock()
}

// movie returns the TMDB ID for a movie match request, or "" if unknown.
func (id *identifier) movie(ctx context.Context, h hints) (string, string, error) {
	tmdb, tvdb := tagIDs(h)
	if tmdb != "" {
		return tmdb, "file name tag", nil
	}
	title, year := splitYear(h.Title, h.Year)
	key := fmt.Sprintf("movie:%s:%d:%s", normTitle(title), year, tvdb)
	if v, ok := id.cached(key); ok {
		return v, "cached lookup", nil
	}
	if tvdb != "" {
		found, err := id.mediux.movieByTVDB(ctx, tvdb)
		if err != nil {
			return "", "", err
		}
		if found != "" {
			id.remember(key, found)
			return found, "TVDB tag", nil
		}
	}
	if title == "" {
		return "", "", nil
	}
	list, err := id.mediux.searchMovies(ctx, title)
	if err != nil {
		return "", "", err
	}
	found := pickTitle(list, title, year)
	id.remember(key, found)
	return found, "title search", nil
}

// show returns the TMDB ID for a show, given its title and any hints.
func (id *identifier) show(ctx context.Context, title string, h hints) (string, string, error) {
	tmdb, tvdb := tagIDs(h)
	if tmdb != "" {
		return tmdb, "file name tag", nil
	}
	title, year := splitYear(title, h.Year)
	if year == 0 {
		// Season and episode requests carry no year, but Sonarr-style file
		// names do ("1923 (2022) - S02E07"), which keeps remakes apart.
		year = fileYear(h.Filename)
	}
	// Same-title shows from different years must never share a cached ID.
	key := fmt.Sprintf("show:%s:%d", normTitle(title), year)
	if tvdb == "" {
		if v, ok := id.cached(key); ok {
			return v, "cached lookup", nil
		}
	}
	remember := func(v string) { id.remember(key, v) }
	how := "TVDB tag"
	if tvdb == "" && title != "" {
		tvdb = id.tvmazeTVDB(ctx, title, year)
		how = "title search (TVmaze)"
	}
	if tvdb != "" {
		found, err := id.mediux.showByTVDB(ctx, tvdb)
		if err != nil {
			return "", "", err
		}
		if found != "" {
			remember(found)
			return found, how, nil
		}
	}
	if title == "" {
		return "", "", nil
	}
	list, err := id.mediux.searchShows(ctx, title)
	if err != nil {
		return "", "", err
	}
	found := pickTitle(list, title, year)
	remember(found)
	return found, "title search (Mediux)", nil
}

// tvmazeTVDB looks a show up on TVmaze (free, no key) to get its TVDB ID.
func (id *identifier) tvmazeTVDB(ctx context.Context, title string, year int) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id.tvmaze+"/search/shows?q="+url.QueryEscape(title), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "plex-plugin-mediux")
	res, err := id.http.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return ""
	}
	var results []struct {
		Show struct {
			Name      string `json:"name"`
			Premiered string `json:"premiered"`
			Externals struct {
				TVDB *int `json:"thetvdb"`
			} `json:"externals"`
		} `json:"show"`
	}
	if json.Unmarshal(raw, &results) != nil {
		return ""
	}
	var list []titleMatch
	for _, r := range results {
		if r.Show.Externals.TVDB != nil {
			list = append(list, titleMatch{ID: strconv.Itoa(*r.Show.Externals.TVDB), Title: r.Show.Name, Date: r.Show.Premiered})
		}
	}
	return pickTitle(list, title, year)
}
