package main

// Mediux API client. The GraphQL queries, response parsing, collection handling
// and error redaction are adapted from Pukabyte's silo-plugin-mediux
// (https://github.com/Pukabyte/silo-plugin-mediux, AGPL-3.0-or-later).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const graphQLEndpoint = "https://images.mediux.io/graphql"
const imageHost = "https://images.mediux.io/assets/"

// errNoToken means the settings page hasn't been filled in yet.
var errNoToken = errors.New("Mediux API token is not configured: add it on the settings page")

var assetID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// Artwork kinds.
const (
	kindPoster   = "poster"
	kindBackdrop = "backdrop"
	kindLogo     = "logo"
	kindSeason   = "season"
	kindStill    = "still" // episode title card
)

type artwork struct {
	ID, Creator, Kind, SetID string
	Season, Episode          int
	Popularity               float64
	Modified                 time.Time
	Rank                     int
	Language                 string
}

type set struct {
	ID, Creator string
	Popularity  float64
	Art         []artwork
	Collection  string // collection name, for collection sets
}

type bundle struct{ Sets []set }

type mediaType string

const (
	typeMovie mediaType = "movie"
	typeShow  mediaType = "show"
)

type cacheEntry struct {
	b     bundle
	until time.Time
}

type flight struct {
	done chan struct{}
	b    bundle
	err  error
}

type mediuxClient struct {
	settings *settingsStore
	endpoint string
	http     *http.Client
	sem      chan struct{}

	mu         sync.Mutex
	cache      map[string]cacheEntry
	flights    map[string]*flight
	generation uint64
}

func newMediuxClient(store *settingsStore) *mediuxClient {
	c := &mediuxClient{
		settings: store,
		endpoint: graphQLEndpoint,
		http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		sem:     make(chan struct{}, 4),
		cache:   map[string]cacheEntry{},
		flights: map[string]*flight{},
	}
	store.onChange(c.invalidate)
	return c
}

// invalidate drops cached artwork so new settings apply on the next refresh.
func (c *mediuxClient) invalidate() {
	c.mu.Lock()
	c.cache = map[string]cacheEntry{}
	c.generation++
	c.mu.Unlock()
}

// query runs one GraphQL request and decodes its data into out.
func (c *mediuxClient) query(ctx context.Context, operation, q string, vars map[string]any, out any) error {
	return c.queryWithToken(ctx, c.settings.get().Token, operation, q, vars, out)
}

func (c *mediuxClient) queryWithToken(ctx context.Context, token, operation, q string, vars map[string]any, out any) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errNoToken
	}
	body, _ := json.Marshal(map[string]any{"query": q, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	// Mediux sits behind Cloudflare, which blocks requests without a clear client name.
	req.Header.Set("User-Agent", "plex-plugin-mediux/"+version)
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.sem }()
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Mediux request failed (%s): %w", operation, err)
	}
	defer res.Body.Close()
	const maxResponse = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(raw) > maxResponse {
		return errors.New("Mediux API response exceeds size limit")
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return fmt.Errorf("Mediux rejected the API token (status %d)", res.StatusCode)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("Mediux API status %d (%s)", res.StatusCode, operation)
	}
	var g struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return err
	}
	if len(g.Errors) > 0 {
		return graphqlError(g.Errors, operation, token)
	}
	return json.Unmarshal(g.Data, out)
}

const showQuery = `query($tmdb_id: ID!){shows_by_id(id:$tmdb_id){id show_sets(limit:-1){ id popularity user_created{username} show_poster(limit:-1){id modified_on language{display_name}} show_backdrop(limit:-1){id modified_on language{display_name}} season_posters(limit:-1){ id modified_on language{display_name} season{season_number}} titlecards(limit:-1){id modified_on language{display_name} episode{episode_number season_id{season_number}}} files(limit:-1){ id file_type modified_on language{display_name} }}}}`
const movieQuery = `query($tmdb_id: ID!){movies_by_id(id:$tmdb_id){id movie_sets(limit:-1){id popularity user_created{username} movie_poster(limit:-1){id modified_on language{display_name}} movie_backdrop(limit:-1){id modified_on language{display_name}} files(limit:-1){id file_type modified_on language{display_name}}} posters(limit:-1,filter:{collection_set:{id:{_nnull:true}}}){id modified_on language{display_name} movie{id} collection_set{id popularity user_created{username} set_title collection_id{collection_name}}} backdrops(limit:-1,filter:{collection_set:{id:{_nnull:true}}}){id modified_on language{display_name} movie{id} collection_set{id popularity user_created{username} set_title collection_id{collection_name}}}}}`

// artwork returns every Mediux set for a movie or show, cached per settings generation.
func (c *mediuxClient) artwork(ctx context.Context, typ mediaType, tmdbID string) (bundle, error) {
	cfg := c.settings.get()
	c.mu.Lock()
	key := fmt.Sprintf("%d:%s:%s", c.generation, typ, tmdbID)
	if e, ok := c.cache[key]; ok && time.Now().Before(e.until) {
		c.mu.Unlock()
		return e.b, nil
	}
	if f := c.flights[key]; f != nil {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.b, f.err
		case <-ctx.Done():
			return bundle{}, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	gen := c.generation
	c.flights[key] = f
	c.mu.Unlock()

	b, err := c.fetchArtwork(ctx, typ, tmdbID)

	c.mu.Lock()
	if err == nil && gen == c.generation && cfg.CacheHours > 0 {
		if len(c.cache) >= 512 {
			c.evictOldestLocked()
		}
		ttl := time.Duration(cfg.CacheHours) * time.Hour
		if len(b.Sets) == 0 && ttl > 15*time.Minute {
			ttl = 15 * time.Minute // look again soon when nothing exists yet
		}
		c.cache[key] = cacheEntry{b, time.Now().Add(ttl)}
	}
	delete(c.flights, key)
	f.b, f.err = b, err
	close(f.done)
	c.mu.Unlock()
	return b, err
}

// forget drops one title's cached artwork so the next lookup asks Mediux again.
func (c *mediuxClient) forget(typ mediaType, tmdbID string) {
	c.mu.Lock()
	delete(c.cache, fmt.Sprintf("%d:%s:%s", c.generation, typ, tmdbID))
	c.mu.Unlock()
}

func (c *mediuxClient) evictOldestLocked() {
	var oldest string
	var until time.Time
	for k, v := range c.cache {
		if oldest == "" || v.until.Before(until) {
			oldest, until = k, v.until
		}
	}
	delete(c.cache, oldest)
}

func (c *mediuxClient) fetchArtwork(ctx context.Context, typ mediaType, tmdbID string) (bundle, error) {
	q, op := movieQuery, "movie artwork"
	if typ == typeShow {
		q, op = showQuery, "show artwork"
	}
	var data json.RawMessage
	if err := c.query(ctx, op, q, map[string]any{"tmdb_id": tmdbID}, &data); err != nil {
		return bundle{}, err
	}
	return parseBundle(data, typ, tmdbID)
}

type mediuxAsset struct {
	ID       string `json:"id"`
	Modified string `json:"modified_on"`
	Language struct {
		DisplayName string `json:"display_name"`
	} `json:"language"`
}

func parseBundle(raw json.RawMessage, typ mediaType, expectedID string) (bundle, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return bundle{}, err
	}
	key, setsKey := "movies_by_id", "movie_sets"
	if typ == typeShow {
		key, setsKey = "shows_by_id", "show_sets"
	}
	if len(root[key]) == 0 || string(root[key]) == "null" {
		return bundle{}, nil
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(root[key], &item); err != nil {
		return bundle{}, err
	}
	var identity struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(root[key], &identity); err != nil {
		return bundle{}, err
	}
	if identity.ID != "" && identity.ID != expectedID {
		return bundle{}, errors.New("Mediux identity mismatch")
	}
	var rows []struct {
		ID         string  `json:"id"`
		Popularity float64 `json:"popularity"`
		User       struct {
			Username string `json:"username"`
		} `json:"user_created"`
		MoviePoster   []mediuxAsset `json:"movie_poster"`
		MovieBackdrop []mediuxAsset `json:"movie_backdrop"`
		ShowPoster    []mediuxAsset `json:"show_poster"`
		ShowBackdrop  []mediuxAsset `json:"show_backdrop"`
		SeasonPosters []struct {
			mediuxAsset
			Season *struct {
				Number int `json:"season_number"`
			} `json:"season"`
		} `json:"season_posters"`
		Titlecards []struct {
			mediuxAsset
			Episode struct {
				Number int `json:"episode_number"`
				Season *struct {
					Number int `json:"season_number"`
				} `json:"season_id"`
			} `json:"episode"`
		} `json:"titlecards"`
		Files []struct {
			mediuxAsset
			Type string `json:"file_type"`
		} `json:"files"`
	}
	if len(item[setsKey]) > 0 && string(item[setsKey]) != "null" {
		if err := json.Unmarshal(item[setsKey], &rows); err != nil {
			return bundle{}, err
		}
	}
	b := bundle{}
	for _, r := range rows {
		s := set{ID: r.ID, Creator: r.User.Username, Popularity: r.Popularity}
		add := func(a mediuxAsset, kind string, season, episode int) {
			if a.ID == "" {
				return
			}
			at, _ := time.Parse(time.RFC3339Nano, a.Modified)
			s.Art = append(s.Art, artwork{ID: a.ID, Creator: r.User.Username, Kind: kind, SetID: r.ID,
				Season: season, Episode: episode, Popularity: r.Popularity, Modified: at,
				Language: normalizeLanguage(a.Language.DisplayName)})
		}
		for _, a := range r.MoviePoster {
			add(a, kindPoster, 0, 0)
		}
		for _, a := range r.MovieBackdrop {
			add(a, kindBackdrop, 0, 0)
		}
		for _, a := range r.ShowPoster {
			add(a, kindPoster, 0, 0)
		}
		for _, a := range r.ShowBackdrop {
			add(a, kindBackdrop, 0, 0)
		}
		for _, a := range r.SeasonPosters {
			if a.Season != nil {
				add(a.mediuxAsset, kindSeason, a.Season.Number, 0)
			}
		}
		for _, a := range r.Titlecards {
			if a.Episode.Season != nil {
				add(a.mediuxAsset, kindStill, a.Episode.Season.Number, a.Episode.Number)
			}
		}
		for _, a := range r.Files {
			if a.Type == "logo" {
				add(a.mediuxAsset, kindLogo, 0, 0)
			}
		}
		b.Sets = append(b.Sets, s)
	}
	if typ == typeMovie {
		collections, err := movieCollectionSets(item, expectedID)
		if err != nil {
			return bundle{}, err
		}
		b.Sets = append(b.Sets, collections...)
	}
	return b, nil
}

// movieCollectionSets groups this movie's collection assets into the same
// candidates as standalone sets. Collection covers and other movies are ignored.
func movieCollectionSets(item map[string]json.RawMessage, movieID string) ([]set, error) {
	var sets []set
	indices := map[string]int{}
	for _, scope := range []struct{ field, kind string }{{"posters", kindPoster}, {"backdrops", kindBackdrop}} {
		if len(item[scope.field]) == 0 || string(item[scope.field]) == "null" {
			continue
		}
		var assets []struct {
			mediuxAsset
			Movie *struct {
				ID string `json:"id"`
			} `json:"movie"`
			Set *struct {
				ID         string  `json:"id"`
				Popularity float64 `json:"popularity"`
				User       struct {
					Username string `json:"username"`
				} `json:"user_created"`
				Title      string `json:"set_title"`
				Collection *struct {
					Name string `json:"collection_name"`
				} `json:"collection_id"`
			} `json:"collection_set"`
		}
		if err := json.Unmarshal(item[scope.field], &assets); err != nil {
			return nil, fmt.Errorf("Mediux collection %s: %w", scope.field, err)
		}
		for _, a := range assets {
			if a.ID == "" || a.Movie == nil || a.Movie.ID != movieID || a.Set == nil || a.Set.ID == "" {
				continue
			}
			i, found := indices[a.Set.ID]
			if !found {
				i = len(sets)
				indices[a.Set.ID] = i
				name := a.Set.Title
				if a.Set.Collection != nil && a.Set.Collection.Name != "" {
					name = a.Set.Collection.Name
				}
				if name == "" {
					name = "Collection"
				}
				sets = append(sets, set{ID: a.Set.ID, Creator: a.Set.User.Username, Popularity: a.Set.Popularity, Collection: name})
			}
			modified, _ := time.Parse(time.RFC3339Nano, a.Modified)
			sets[i].Art = append(sets[i].Art, artwork{ID: a.ID, Creator: a.Set.User.Username, Kind: scope.kind,
				SetID: a.Set.ID, Popularity: a.Set.Popularity, Modified: modified,
				Language: normalizeLanguage(a.Language.DisplayName)})
		}
	}
	return sets, nil
}

// showByTVDB maps a TVDB show ID to the TMDB ID Mediux uses.
func (c *mediuxClient) showByTVDB(ctx context.Context, tvdbID string) (string, error) {
	var out struct {
		Shows []struct {
			ID string `json:"id"`
		} `json:"shows"`
	}
	q := `query($tvdb_id:String!){shows(filter:{tvdb_id:{_eq:$tvdb_id}}){id}}`
	if err := c.query(ctx, "TVDB show lookup", q, map[string]any{"tvdb_id": tvdbID}, &out); err != nil {
		return "", err
	}
	if len(out.Shows) == 0 {
		return "", nil
	}
	return out.Shows[0].ID, nil
}

// movieByTVDB maps a TVDB movie ID to the TMDB ID Mediux uses.
func (c *mediuxClient) movieByTVDB(ctx context.Context, tvdbID string) (string, error) {
	var out struct {
		Movies []struct {
			ID string `json:"id"`
		} `json:"movies"`
	}
	q := `query($tvdb_id:String!){movies(filter:{tvdb_id:{_eq:$tvdb_id}}){id}}`
	if err := c.query(ctx, "TVDB movie lookup", q, map[string]any{"tvdb_id": tvdbID}, &out); err != nil {
		return "", err
	}
	if len(out.Movies) == 0 {
		return "", nil
	}
	return out.Movies[0].ID, nil
}

type titleMatch struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Date  string `json:"date"`
}

// searchMovies finds Mediux movies by title. Used only when the file name has no ID.
func (c *mediuxClient) searchMovies(ctx context.Context, title string) ([]titleMatch, error) {
	return c.searchTitles(ctx, "movies", "release_date", title)
}

// searchShows finds Mediux shows by title. Used only when other lookups fail.
func (c *mediuxClient) searchShows(ctx context.Context, title string) ([]titleMatch, error) {
	return c.searchTitles(ctx, "shows", "first_air_date", title)
}

// searchTitles looks a title up four ways in one request, best first:
// exact (Mediux ignores case), exact with or without a leading "The",
// starting with the text, then containing it. A "contains" search alone
// misses titles like "1917", whose first 25 hits are other films with 1917 in
// the name, and "The Evil Dead" for "Evil Dead".
func (c *mediuxClient) searchTitles(ctx context.Context, collection, dateField, title string) ([]titleMatch, error) {
	alt := "The " + title
	if strings.HasPrefix(strings.ToLower(title), "the ") {
		alt = strings.TrimSpace(title[4:])
	}
	fields := "{id title " + dateField + "}"
	q := "query($q:String!,$alt:String!){" +
		"exact:" + collection + "(limit:50,filter:{title:{_eq:$q}})" + fields +
		" alt:" + collection + "(limit:50,filter:{title:{_eq:$alt}})" + fields +
		" starts:" + collection + "(limit:25,filter:{title:{_istarts_with:$q}})" + fields +
		" near:" + collection + "(limit:25,filter:{title:{_icontains:$q}})" + fields + "}"
	var out map[string][]map[string]any
	if err := c.query(ctx, collection+" title search", q, map[string]any{"q": title, "alt": alt}, &out); err != nil {
		return nil, err
	}
	var res []titleMatch
	seen := map[string]bool{}
	for _, group := range []string{"exact", "alt", "starts", "near"} {
		for _, m := range out[group] {
			id, _ := m["id"].(string)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			t, _ := m["title"].(string)
			d, _ := m[dateField].(string)
			res = append(res, titleMatch{id, t, d})
		}
	}
	return res, nil
}

const creatorQuery = `query($query:String!){contributors(limit:51,sort:["username"],filter:{username:{_icontains:$query}}){username}}`

// creators searches Mediux usernames for the settings page.
func (c *mediuxClient) creators(ctx context.Context, q string) ([]string, error) {
	var out struct {
		Contributors []struct {
			Username string `json:"username"`
		} `json:"contributors"`
	}
	if err := c.query(ctx, "creator search", creatorQuery, map[string]any{"query": q}, &out); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	names := []string{}
	for _, x := range out.Contributors {
		name := strings.TrimSpace(x.Username)
		k := strings.ToLower(name)
		if name == "" || len(name) > 128 || strings.ContainsAny(name, ",\r\n") || seen[k] {
			continue
		}
		seen[k] = true
		names = append(names, name)
		if len(names) == 50 {
			break
		}
	}
	return names, nil
}

// checkToken confirms a token works with a tiny query.
func (c *mediuxClient) checkToken(ctx context.Context, token string) error {
	var out json.RawMessage
	return c.queryWithToken(ctx, token, "connection check", creatorQuery, map[string]any{"query": "ab"}, &out)
}

// imageURL is the Mediux address for an asset. The version changes whenever
// the artwork changes, so Plex downloads the new image.
func imageURL(a artwork, quality string) string {
	v := a.Modified.UTC().Format("20060102150405")
	if a.Modified.IsZero() {
		v = "0"
	}
	values := url.Values{}
	if quality != "original" && a.Kind != kindLogo {
		values.Set("key", "jpg")
	}
	values.Set("v", v)
	return imageHost + a.ID + "?" + values.Encode()
}

var languageNames = map[string]string{"english": "en", "spanish": "es", "french": "fr", "german": "de",
	"italian": "it", "portuguese": "pt", "brazilian portuguese": "pt-br", "japanese": "ja", "korean": "ko",
	"chinese": "zh", "simplified chinese": "zh-cn", "traditional chinese": "zh-tw", "russian": "ru",
	"arabic": "ar", "hindi": "hi", "dutch": "nl", "polish": "pl", "turkish": "tr", "swedish": "sv",
	"norwegian": "no", "danish": "da", "finnish": "fi"}

func normalizeLanguage(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if len(n) == 2 {
		return n
	}
	return languageNames[n]
}

// plexLanguage turns Plex's "en-US" into the codes Mediux artwork uses.
func plexLanguage(header string) string {
	l := strings.ToLower(strings.TrimSpace(header))
	switch l {
	case "pt-br", "zh-cn", "zh-tw":
		return l
	}
	if i := strings.IndexByte(l, '-'); i > 0 {
		l = l[:i]
	}
	if len(l) == 2 {
		return l
	}
	return ""
}

var bearerCredential = regexp.MustCompile(`(?i)\bBearer\s+(?:"[^"]*"|'[^']*'|[^\s"'<>,;]+)`)

// graphqlError keeps actionable upstream diagnostics without leaking the token.
func graphqlError(raw []json.RawMessage, operation, token string) error {
	clean := func(value string) string {
		if token != "" {
			value = strings.ReplaceAll(value, token, "[REDACTED]")
			for _, secret := range []string{url.QueryEscape(token), url.PathEscape(token)} {
				if secret != token {
					value = regexp.MustCompile("(?i)"+regexp.QuoteMeta(secret)).ReplaceAllLiteralString(value, "[REDACTED]")
				}
			}
		}
		value = bearerCredential.ReplaceAllString(value, "Bearer [REDACTED]")
		value = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return ' '
			}
			return r
		}, value)
		value = strings.Join(strings.Fields(value), " ")
		if runes := []rune(value); len(runes) > 512 {
			value = string(runes[:512]) + "…"
		}
		return value
	}
	var details []string
	for _, entry := range raw[:min(len(raw), 3)] {
		var item struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		}
		if json.Unmarshal(entry, &item) != nil {
			continue
		}
		detail := clean(item.Message)
		if code := clean(item.Extensions.Code); code != "" {
			detail = code + ": " + detail
		}
		if strings.TrimSpace(detail) != "" {
			details = append(details, detail)
		}
	}
	message := "Mediux API returned errors (" + operation + ")"
	if len(details) > 0 {
		message += ": " + strings.Join(details, "; ")
	}
	return errors.New(message)
}

type structureEntry struct {
	seasons map[int][]int
	err     error
	until   time.Time
}

var (
	structureMu    sync.Mutex
	structureCache = map[string]structureEntry{}
)

// showStructure lists a show's seasons and episodes as Mediux knows them
// (TMDB numbering). Errors are cached briefly; callers fall back to the
// seasons and episodes that have artwork.
func (c *mediuxClient) showStructure(ctx context.Context, tmdbID string) (map[int][]int, error) {
	structureMu.Lock()
	if e, ok := structureCache[tmdbID]; ok && time.Now().Before(e.until) {
		structureMu.Unlock()
		return e.seasons, e.err
	}
	structureMu.Unlock()
	var out struct {
		Show *struct {
			Seasons []struct {
				Number   int `json:"season_number"`
				Episodes []struct {
					Number int `json:"episode_number"`
				} `json:"episodes"`
			} `json:"seasons"`
		} `json:"shows_by_id"`
	}
	q := `query($tmdb_id: ID!){shows_by_id(id:$tmdb_id){seasons(limit:-1){season_number episodes(limit:-1){episode_number}}}}`
	err := c.query(ctx, "show structure", q, map[string]any{"tmdb_id": tmdbID}, &out)
	seasons := map[int][]int{}
	if err == nil && out.Show != nil {
		for _, s := range out.Show.Seasons {
			for _, e := range s.Episodes {
				seasons[s.Number] = append(seasons[s.Number], e.Number)
			}
			if _, ok := seasons[s.Number]; !ok {
				seasons[s.Number] = nil
			}
		}
	}
	ttl := 12 * time.Hour
	if err != nil {
		ttl = 10 * time.Minute
	}
	structureMu.Lock()
	if len(structureCache) > 5000 {
		structureCache = map[string]structureEntry{}
	}
	structureCache[tmdbID] = structureEntry{seasons, err, time.Now().Add(ttl)}
	structureMu.Unlock()
	return seasons, err
}

// titleOf returns a movie or show's name and year as Mediux lists it.
func (c *mediuxClient) titleOf(ctx context.Context, typ mediaType, tmdbID string) (string, int) {
	field, date := "movies_by_id", "release_date"
	if typ == typeShow {
		field, date = "shows_by_id", "first_air_date"
	}
	var out map[string]*struct {
		Title string `json:"title"`
		Movie string `json:"release_date"`
		Show  string `json:"first_air_date"`
	}
	q := "query($id: ID!){" + field + "(id:$id){title " + date + "}}"
	if err := c.query(ctx, "title lookup", q, map[string]any{"id": tmdbID}, &out); err != nil || out[field] == nil {
		return "", 0
	}
	t := out[field]
	return t.Title, yearOf(t.Movie + t.Show)
}

// collectionMovie is one movie a collection set has artwork for.
type collectionMovie struct {
	TMDB          string `json:"tmdb"`
	Title         string `json:"title"`
	Year          int    `json:"year,omitempty"`
	Poster        bool   `json:"poster"`
	Background    bool   `json:"background"`
	PosterURL     string `json:"poster_url,omitempty"`
	BackgroundURL string `json:"background_url,omitempty"`
}

var (
	collectionMu    sync.Mutex
	collectionCache = map[string]struct {
		movies []collectionMovie
		until  time.Time
	}{}
)

// collectionMovies lists the movies a collection set covers, from the movies
// its posters and backgrounds belong to (Mediux's own collection movie lists
// are often empty).
func (c *mediuxClient) collectionMovies(ctx context.Context, setID string) ([]collectionMovie, error) {
	collectionMu.Lock()
	if e, ok := collectionCache[setID]; ok && time.Now().Before(e.until) {
		collectionMu.Unlock()
		return e.movies, nil
	}
	collectionMu.Unlock()
	type file struct {
		ID       string `json:"id"`
		Modified string `json:"modified_on"`
		Movie    *struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Release string `json:"release_date"`
		} `json:"movie"`
	}
	var out struct {
		Set *struct {
			Posters   []file `json:"movie_posters"`
			Backdrops []file `json:"movie_backdrops"`
		} `json:"collection_sets_by_id"`
	}
	q := `query($id: ID!){collection_sets_by_id(id:$id){movie_posters(limit:-1){id modified_on movie{id title release_date}} movie_backdrops(limit:-1){id modified_on movie{id title release_date}}}}`
	if err := c.query(ctx, "collection set", q, map[string]any{"id": setID}, &out); err != nil {
		return nil, err
	}
	byID := map[string]*collectionMovie{}
	var order []string
	add := func(f file, poster bool) {
		if f.Movie == nil || f.Movie.ID == "" {
			return
		}
		m := byID[f.Movie.ID]
		if m == nil {
			m = &collectionMovie{TMDB: f.Movie.ID, Title: f.Movie.Title, Year: yearOf(f.Movie.Release)}
			byID[f.Movie.ID] = m
			order = append(order, f.Movie.ID)
		}
		at, _ := time.Parse(time.RFC3339Nano, f.Modified)
		if poster {
			m.Poster = true
			if m.PosterURL == "" && assetID.MatchString(f.ID) {
				m.PosterURL = imageURL(artwork{ID: f.ID, Kind: kindPoster, Modified: at}, "optimized")
			}
		} else {
			m.Background = true
			if m.BackgroundURL == "" && assetID.MatchString(f.ID) {
				m.BackgroundURL = imageURL(artwork{ID: f.ID, Kind: kindBackdrop, Modified: at}, "optimized")
			}
		}
	}
	if out.Set != nil {
		for _, f := range out.Set.Posters {
			add(f, true)
		}
		for _, f := range out.Set.Backdrops {
			add(f, false)
		}
	}
	movies := make([]collectionMovie, 0, len(order))
	for _, id := range order {
		movies = append(movies, *byID[id])
	}
	sort.SliceStable(movies, func(i, j int) bool {
		if movies[i].Year != movies[j].Year {
			return movies[i].Year < movies[j].Year
		}
		return movies[i].Title < movies[j].Title
	})
	collectionMu.Lock()
	collectionCache[setID] = struct {
		movies []collectionMovie
		until  time.Time
	}{movies, time.Now().Add(6 * time.Hour)}
	collectionMu.Unlock()
	return movies, nil
}
