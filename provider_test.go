package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// uuid builds a valid Mediux-style asset ID from a small number.
func uuid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func asset(n int) map[string]any {
	return map[string]any{"id": uuid(n), "modified_on": "2026-01-02T03:04:05Z", "language": nil}
}

// extraCard adds a new S2E3 title card to the fake Mediux, as if a creator
// had just uploaded it.
var extraCard atomic.Bool

// fakeMediux answers the GraphQL queries the provider sends.
func fakeMediux(t *testing.T) *httptest.Server {
	movie := map[string]any{"movies_by_id": map[string]any{
		"id": "291870",
		"movie_sets": []any{
			map[string]any{"id": "set-a", "popularity": 50, "user_created": map[string]any{"username": "LowPriority"},
				"movie_poster": []any{asset(1)}, "movie_backdrop": []any{asset(2)}, "files": []any{}},
			map[string]any{"id": "set-b", "popularity": 5, "user_created": map[string]any{"username": "willtong93"},
				"movie_poster": []any{asset(3)}, "movie_backdrop": []any{asset(4)},
				"files": []any{map[string]any{"id": uuid(5), "file_type": "logo", "modified_on": "2026-01-02T03:04:05Z"}}},
			map[string]any{"id": "set-c", "popularity": 99, "user_created": map[string]any{"username": "MrMagixx"},
				"movie_poster": []any{asset(6)}, "movie_backdrop": []any{}, "files": []any{}},
		},
		"posters": []any{}, "backdrops": []any{},
	}}
	season := func(n, s int) map[string]any {
		a := asset(n)
		a["season"] = map[string]any{"season_number": s}
		return a
	}
	card := func(n, s, e int) map[string]any {
		a := asset(n)
		a["episode"] = map[string]any{"episode_number": e, "season_id": map[string]any{"season_number": s}}
		return a
	}
	show := map[string]any{"shows_by_id": map[string]any{
		"id": "194764",
		"show_sets": []any{
			// Complete set (both seasons, all four title cards) from an unlisted creator.
			map[string]any{"id": "complete", "popularity": 10, "user_created": map[string]any{"username": "Someone"},
				"show_poster": []any{asset(10)}, "show_backdrop": []any{asset(11)},
				"season_posters": []any{season(12, 1), season(13, 2)},
				"titlecards":     []any{card(14, 1, 1), card(15, 1, 2), card(16, 2, 1), card(17, 2, 2)},
				"files":          []any{}},
			// Partial set (season 1 only) from the top-priority creator.
			map[string]any{"id": "favorite", "popularity": 1, "user_created": map[string]any{"username": "willtong93"},
				"show_poster": []any{asset(20)}, "show_backdrop": []any{},
				"season_posters": []any{season(21, 1)},
				"titlecards":     []any{card(22, 1, 1), card(23, 1, 2)},
				"files":          []any{}},
		},
	}}
	structure := map[string]any{"shows_by_id": map[string]any{"seasons": []any{
		map[string]any{"season_number": 1, "episodes": []any{map[string]any{"episode_number": 1}, map[string]any{"episode_number": 2}}},
		map[string]any{"season_number": 2, "episodes": []any{map[string]any{"episode_number": 1}, map[string]any{"episode_number": 2}, map[string]any{"episode_number": 3}}},
	}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		var data any
		switch {
		case strings.Contains(body.Query, "movies_by_id"):
			if body.Variables["tmdb_id"] == "291870" {
				data = movie
			} else {
				data = map[string]any{"movies_by_id": nil}
			}
		case strings.Contains(body.Query, "shows_by_id(id:$tmdb_id){seasons"):
			data = structure
		case strings.Contains(body.Query, "shows_by_id"):
			// Answer for whichever show was asked about.
			inner := map[string]any{}
			for k, v := range show["shows_by_id"].(map[string]any) {
				inner[k] = v
			}
			inner["id"] = body.Variables["tmdb_id"]
			if extraCard.Load() {
				sets := append([]any{}, inner["show_sets"].([]any)...)
				first := map[string]any{}
				for k, v := range sets[0].(map[string]any) {
					first[k] = v
				}
				first["titlecards"] = append(append([]any{}, first["titlecards"].([]any)...), card(18, 2, 3))
				sets[0] = first
				inner["show_sets"] = sets
			}
			data = map[string]any{"shows_by_id": inner}
		case strings.Contains(body.Query, "shows(filter:{tvdb_id"):
			tmdbByTVDB := map[any]string{"416491": "194764", "71173": "501", "73545": "1972"}
			if id, ok := tmdbByTVDB[body.Variables["tvdb_id"]]; ok {
				data = map[string]any{"shows": []any{map[string]any{"id": id}}}
			} else {
				data = map[string]any{"shows": []any{}}
			}
		case strings.Contains(body.Query, "exact:movies"):
			// Exact matches ignore case; "near" holds titles containing the text.
			var exact []any
			if strings.EqualFold(fmt.Sprint(body.Variables["q"]), "Dirty Grandpa") {
				exact = []any{map[string]any{"id": "291870", "title": "Dirty Grandpa", "release_date": "2016-01-22"}}
			}
			data = map[string]any{"exact": exact, "alt": []any{}, "starts": []any{}, "near": []any{
				map[string]any{"id": "1", "title": "Dirty Grandpa Returns", "release_date": "2030-01-01"},
				map[string]any{"id": "291870", "title": "Dirty Grandpa", "release_date": "2016-01-22"},
			}}
		case strings.Contains(body.Query, "collection_sets_by_id"):
			movie := func(id, title string) map[string]any {
				return map[string]any{"movie": map[string]any{"id": id, "title": title, "release_date": "2016-01-22"}}
			}
			data = map[string]any{"collection_sets_by_id": map[string]any{
				"movie_posters":   []any{movie("291870", "Dirty Grandpa"), movie("555", "Dirty Grandpa 2")},
				"movie_backdrops": []any{movie("291870", "Dirty Grandpa")}}}
		case strings.Contains(body.Query, "exact:shows"):
			data = map[string]any{"exact": []any{}, "alt": []any{}, "starts": []any{}, "near": []any{}}
		case strings.Contains(body.Query, "contributors"):
			data = map[string]any{"contributors": []any{map[string]any{"username": "willtong93"}}}
		default:
			t.Errorf("unexpected query %s", body.Query)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

func fakeTVmaze() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{
			map[string]any{"show": map[string]any{"name": "1923", "premiered": "2022-12-18", "externals": map[string]any{"thetvdb": 416491}}},
			map[string]any{"show": map[string]any{"name": "Battlestar Galactica", "premiered": "1978-09-17", "externals": map[string]any{"thetvdb": 71173}}},
			map[string]any{"show": map[string]any{"name": "Battlestar Galactica", "premiered": "2004-10-18", "externals": map[string]any{"thetvdb": 73545}}},
		})
	}))
}

type harness struct {
	server   *httptest.Server
	store    *settingsStore
	activity *activityLog
	app      *app
}

func newHarness(t *testing.T, edit func(*settings)) *harness {
	t.Helper()
	mx, tv := fakeMediux(t), fakeTVmaze()
	t.Cleanup(mx.Close)
	t.Cleanup(tv.Close)
	store, err := loadSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := store.get()
	s.Token = "good-token"
	s.PriorityCreators = []string{"willtong93"}
	s.ExcludedCreators = []string{"MrMagixx"}
	if edit != nil {
		edit(&s)
	}
	if err := store.save(s); err != nil {
		t.Fatal(err)
	}
	extraCard.Store(false)
	a := newApp(store, t.TempDir())
	a.mediux.endpoint = mx.URL
	a.ids.tvmaze = tv.URL
	mux := a.mux
	activity := a.activity
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{srv, store, activity, a}
}

func (h *harness) post(t *testing.T, path string, body any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	res, err := http.Post(h.server.URL+path, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, res)
}

func (h *harness) get(t *testing.T, path string) map[string]any {
	t.Helper()
	res, err := http.Get(h.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, res)
}

func decode(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("status %d: %v", res.StatusCode, err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %v", res.StatusCode, out)
	}
	return out
}

func metadata(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	mc := out["MediaContainer"].(map[string]any)
	var items []map[string]any
	for _, x := range mc["Metadata"].([]any) {
		items = append(items, x.(map[string]any))
	}
	return items
}

func assetOf(url any) string {
	s, _ := url.(string)
	s = strings.TrimPrefix(s, imageHost)
	id, _, _ := strings.Cut(s, "?")
	return id
}

func TestMovieMatchUsesFileNameTagAndCreatorPriority(t *testing.T) {
	h := newHarness(t, nil)
	out := h.post(t, "/movies/library/metadata/matches", map[string]any{
		"type": 1, "title": "Dirty Grandpa", "year": 2016,
		"filename": "Dirty Grandpa (2016) {tmdb-291870}{edition-Unrated} [Bluray-1080p].mkv",
	})
	items := metadata(t, out)
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %v", items)
	}
	m := items[0]
	if m["ratingKey"] != "tmdb-291870" || m["type"] != "movie" {
		t.Fatalf("unexpected item %v", m)
	}
	for _, field := range []string{"title", "originallyAvailableAt", "year", "summary"} {
		if _, ok := m[field]; ok {
			t.Errorf("artwork-only item must not send %s", field)
		}
	}
	// willtong93 is prioritized, so set-b wins over the more popular set-a.
	if assetOf(m["thumb"]) != uuid(3) || assetOf(m["art"]) != uuid(4) {
		t.Errorf("poster/background = %v / %v, want set-b", m["thumb"], m["art"])
	}
	if !strings.Contains(m["thumb"].(string), "key=jpg") || !strings.Contains(m["thumb"].(string), "v=20260102030405") {
		t.Errorf("image URL should carry quality and version: %v", m["thumb"])
	}
}

func TestMovieExclusionsAndGallery(t *testing.T) {
	h := newHarness(t, nil)
	out := h.get(t, "/movies/library/metadata/tmdb-291870/images")
	mc := out["MediaContainer"].(map[string]any)
	var posters []string
	for _, x := range mc["Image"].([]any) {
		img := x.(map[string]any)
		if img["type"] == "coverPoster" {
			posters = append(posters, assetOf(img["url"]))
		}
	}
	// MrMagixx is excluded; willtong93 first, then the rest by popularity.
	if strings.Join(posters, ",") != uuid(3)+","+uuid(1) {
		t.Errorf("gallery posters = %v", posters)
	}
}

func TestMovieTitleFallbackNeedsExactTitle(t *testing.T) {
	h := newHarness(t, nil)
	items := metadata(t, h.post(t, "/movies/library/metadata/matches", map[string]any{"type": 1, "title": "Dirty Grandpa", "year": 2016, "filename": "Dirty Grandpa.mkv"}))
	if len(items) != 1 || items[0]["ratingKey"] != "tmdb-291870" {
		t.Fatalf("title search should pick the exact title: %v", items)
	}
	items = metadata(t, h.post(t, "/movies/library/metadata/matches", map[string]any{"type": 1, "title": "Dirty", "year": 2016, "filename": "Dirty.mkv"}))
	if len(items) != 0 {
		t.Fatalf("partial title must not match: %v", items)
	}
}

func TestOnlyPriorityCreators(t *testing.T) {
	h := newHarness(t, func(s *settings) { s.OnlyPriority = true; s.PriorityCreators = []string{"nobody"} })
	items := metadata(t, h.get(t, "/movies/library/metadata/tmdb-291870"))
	if _, ok := items[0]["thumb"]; ok {
		t.Fatalf("no prioritized creator has art, so no poster should be sent: %v", items[0])
	}
}

func TestShowMatchByTitleWithChildren(t *testing.T) {
	h := newHarness(t, nil)
	items := metadata(t, h.post(t, "/tv/library/metadata/matches", map[string]any{
		"type": 2, "title": "1923", "includeChildren": 1,
		"filename": "Season 02/1923 (2022) - S02E07 - A Dream and a Memory.mkv",
	}))
	if len(items) != 1 || items[0]["ratingKey"] != "tmdb-194764" {
		t.Fatalf("show match: %v", items)
	}
	kids := items[0]["Children"].(map[string]any)["Metadata"].([]any)
	if len(kids) != 2 {
		t.Fatalf("want 2 seasons, got %d", len(kids))
	}
}

func TestSeasonAndTitleCardConsistency(t *testing.T) {
	cases := []struct {
		name               string
		mode, missing      string
		s2Poster, s2e1Card string // "" means no Mediux image (Plex keeps its own)
		s1Poster           string
	}{
		{"complete set first", modeMostComplete, "Fallback", uuid(13), uuid(16), uuid(12)},
		{"creator first, fall back for gaps", modeCreatorPriority, "Fallback", uuid(13), uuid(16), uuid(21)},
		{"creator first, keep existing for gaps", modeCreatorPriority, "KeepExisting", "", "", uuid(21)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, func(s *settings) {
				s.SeasonConsistency, s.TitlecardConsistency, s.MissingArtwork = c.mode, c.mode, c.missing
			})
			s1 := metadata(t, h.post(t, "/tv/library/metadata/matches", map[string]any{"type": 3, "parentTitle": "1923", "index": 1, "includeChildren": 1}))[0]
			if assetOf(s1["thumb"]) != c.s1Poster {
				t.Errorf("season 1 poster = %v, want %v", s1["thumb"], c.s1Poster)
			}
			s2 := metadata(t, h.post(t, "/tv/library/metadata/matches", map[string]any{"type": 3, "parentTitle": "1923", "index": 2, "includeChildren": 1}))[0]
			if assetOf(s2["thumb"]) != c.s2Poster {
				t.Errorf("season 2 poster = %v, want %v", s2["thumb"], c.s2Poster)
			}
			eps := s2["Children"].(map[string]any)["Metadata"].([]any)
			if len(eps) != 3 {
				t.Fatalf("season 2 should list 3 episodes from the show structure, got %d", len(eps))
			}
			e1 := eps[0].(map[string]any)
			if e1["index"].(float64) != 1 || assetOf(e1["thumb"]) != c.s2e1Card {
				t.Errorf("S2E1 card = %v, want %v", e1["thumb"], c.s2e1Card)
			}
			if _, ok := eps[2].(map[string]any)["thumb"]; ok {
				t.Errorf("S2E3 has no Mediux card, so none should be sent")
			}
		})
	}
}

func TestTVKeysAndGrandchildrenPaging(t *testing.T) {
	h := newHarness(t, nil)
	out := h.get(t, "/tv/library/metadata/tmdb-194764/grandchildren?X-Plex-Container-Start=2&X-Plex-Container-Size=2")
	mc := out["MediaContainer"].(map[string]any)
	if mc["totalSize"].(float64) != 5 || mc["size"].(float64) != 2 {
		t.Fatalf("paging: %v", mc)
	}
	ep := metadata(t, h.get(t, "/tv/library/metadata/tmdb-194764-s1e2"))[0]
	if ep["type"] != "episode" || ep["grandparentRatingKey"] != "tmdb-194764" || ep["parentRatingKey"] != "tmdb-194764-s1" {
		t.Fatalf("episode item: %v", ep)
	}
}

func TestSettingsKeepTokenAndBlockCrossSite(t *testing.T) {
	h := newHarness(t, nil)
	body := `{"priority_creators":["a","A","b"],"excluded_creators":[],"season_consistency":"MostComplete","titlecard_consistency":"MostComplete","missing_artwork":"Fallback","quality":"original","image_delivery":"direct","cache_hours":6}`
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/settings", strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("post without the settings header should be refused, got %d", res.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, h.server.URL+"/api/settings", strings.NewReader(body))
	req.Header.Set("X-Mediux-Settings", "1")
	out := decode(t, mustDo(t, req))
	if out["token_configured"] != true || out["api_token"] != nil {
		t.Fatalf("token must be kept and never returned: %v", out)
	}
	if s := h.store.get(); s.Token != "good-token" || len(s.PriorityCreators) != 2 || s.Quality != "original" {
		t.Fatalf("saved settings: %+v", s)
	}
}

func mustDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestTagParsing(t *testing.T) {
	cases := []struct{ file, tmdb, tvdb string }{
		{"Dirty Grandpa (2016) {tmdb-291870}{edition-Unrated}.mkv", "291870", ""},
		{"Show [tmdbid-123] - S01E01.mkv", "123", ""},
		{"Show {tvdb-416491} - S01E01.mkv", "", "416491"},
		{"Season 02/1923 (2022) - S02E07 - A Dream.mkv", "", ""},
	}
	for _, c := range cases {
		tm, tv := tagIDs(hints{Filename: c.file})
		if tm != c.tmdb || tv != c.tvdb {
			t.Errorf("%s: got %q %q", c.file, tm, tv)
		}
	}
	if title, year := splitYear("1923 (2022)", 0); title != "1923" || year != 2022 {
		t.Errorf("splitYear: %q %d", title, year)
	}
}

func TestSettingsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	store, _ := loadSettings(dir)
	s := store.get()
	s.Token, s.PriorityCreators, s.CacheHours = "secret", []string{"x"}, 3
	if err := store.save(s); err != nil {
		t.Fatal(err)
	}
	again, err := loadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g := again.get(); g.Token != "secret" || g.CacheHours != 3 || g.PriorityCreators[0] != "x" {
		t.Fatalf("reloaded %+v", g)
	}
	_ = time.Now
}

func TestSameTitleShowsFromDifferentYearsStayApart(t *testing.T) {
	h := newHarness(t, nil)
	match := func(kind int, file string) any {
		body := map[string]any{"type": kind, "filename": file}
		if kind == 2 {
			body["title"] = "Battlestar Galactica"
		} else {
			body["parentTitle"], body["index"] = "Battlestar Galactica", 1
		}
		items := metadata(t, h.post(t, "/tv/library/metadata/matches", body))
		if len(items) != 1 {
			t.Fatalf("%s: %v", file, items)
		}
		return items[0]["ratingKey"]
	}
	if got := match(2, "Season 01/Battlestar Galactica (1978) - S01E01.mkv"); got != "tmdb-501" {
		t.Errorf("1978 show = %v", got)
	}
	if got := match(3, "Season 01/Battlestar Galactica (2004) - S01E01.mkv"); got != "tmdb-1972-s1" {
		t.Errorf("2004 season must not reuse the 1978 lookup, got %v", got)
	}
	if got := match(3, "Season 01/Battlestar Galactica (1978) - S01E02.mkv"); got != "tmdb-501-s1" {
		t.Errorf("1978 season = %v", got)
	}
}

// writeAuraDB creates a minimal AURA.db with the tables the provider reads.
func writeAuraDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE MediaItems (id INTEGER PRIMARY KEY, tmdb_id TEXT, library_title TEXT, type TEXT, title TEXT, year INTEGER)`,
		`CREATE TABLE SavedItems (tmdb_id TEXT, library_title TEXT, poster_set_id TEXT, poster_selected BOOLEAN,
			backdrop_selected BOOLEAN, season_poster_selected BOOLEAN, special_season_poster_selected BOOLEAN,
			titlecard_selected BOOLEAN, last_downloaded TEXT)`,
		`CREATE TABLE ImageFiles (image_id TEXT, image_type TEXT, image_season_number INTEGER, image_episode_number INTEGER,
			poster_set_id TEXT, item_tmdb_id TEXT)`,
		`INSERT INTO MediaItems VALUES (1, '291870', 'Movies', 'movie', 'Dirty Grandpa', 2016), (2, '194764', 'TV Shows', 'show', '1923', 2022)`,
		// Movie: poster from LowPriority's set, although willtong93 is prioritized.
		`INSERT INTO SavedItems VALUES ('291870', 'Movies', 'set-a', 1, 0, 0, 0, 0, '2026-01-01')`,
		`INSERT INTO ImageFiles VALUES ('` + uuid(1) + `', 'poster', NULL, NULL, 'set-a', '291870')`,
		// Show: season 1 poster and S1E1 card from the "complete" set, not willtong93's.
		`INSERT INTO SavedItems VALUES ('194764', 'TV Shows', 'complete', 0, 0, 1, 0, 1, '2026-01-02')`,
		`INSERT INTO ImageFiles VALUES ('` + uuid(12) + `', 'season_poster', 1, NULL, 'complete', '194764'),
			('` + uuid(14) + `', 'titlecard', 1, 1, 'complete', '194764')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestAuraPicksWinOverCreatorPriority(t *testing.T) {
	path := t.TempDir() + "/AURA.db"
	writeAuraDB(t, path)
	h := newHarness(t, func(s *settings) {
		s.AuraDB = path
		s.SeasonConsistency, s.TitlecardConsistency = modeCreatorPriority, modeCreatorPriority
	})
	movie := metadata(t, h.get(t, "/movies/library/metadata/tmdb-291870"))[0]
	if assetOf(movie["thumb"]) != uuid(1) {
		t.Errorf("movie poster should be the AURA pick, got %v", movie["thumb"])
	}
	if assetOf(movie["art"]) != uuid(2) {
		t.Errorf("background should come from the AURA poster's set, got %v", movie["art"])
	}
	s1 := metadata(t, h.get(t, "/tv/library/metadata/tmdb-194764-s1?includeChildren=1"))[0]
	if assetOf(s1["thumb"]) != uuid(12) {
		t.Errorf("season 1 poster should be the AURA pick, got %v", s1["thumb"])
	}
	eps := s1["Children"].(map[string]any)["Metadata"].([]any)
	if got := assetOf(eps[0].(map[string]any)["thumb"]); got != uuid(14) {
		t.Errorf("S1E1 card should be the AURA pick, got %v", got)
	}
	// Not pinned: S1E2 still follows creator priority (willtong93's set).
	if got := assetOf(eps[1].(map[string]any)["thumb"]); got != uuid(23) {
		t.Errorf("S1E2 card should follow creator priority, got %v", got)
	}
	out := h.get(t, "/api/aura")
	if st := out["status"].(map[string]any); st["movies"].(float64) != 1 || st["shows"].(float64) != 1 {
		t.Errorf("AURA status: %v", st)
	}
}

func TestMissingAuraDatabaseIsHarmless(t *testing.T) {
	h := newHarness(t, func(s *settings) { s.AuraDB = "/nowhere/AURA.db" })
	movie := metadata(t, h.get(t, "/movies/library/metadata/tmdb-291870"))[0]
	if assetOf(movie["thumb"]) != uuid(3) {
		t.Errorf("without AURA, creator priority applies: %v", movie["thumb"])
	}
	if st := h.get(t, "/api/aura")["status"].(map[string]any); st["found"] != false {
		t.Errorf("status should say not found: %v", st)
	}
}

// fakePlex records refresh requests for the watcher test.
type fakePlex struct {
	mu        sync.Mutex
	refreshed []string
	edits     []string // library edit queries, in order
}

func (f *fakePlex) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "plex-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/":
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"friendlyName": "Unlimited-Primary"}})
		case r.URL.Path == "/library/sections":
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Directory": []any{
				map[string]any{"key": "1", "type": "show"}, map[string]any{"key": "2", "type": "artist"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/library/sections/1/all":
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []any{
				map[string]any{"ratingKey": "777", "Guid": []any{map[string]any{"id": "imdb://tt1"}, map[string]any{"id": "tmdb://194764"}}}}}})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/refresh"):
			f.mu.Lock()
			f.refreshed = append(f.refreshed, r.URL.Path)
			f.mu.Unlock()
		case r.Method == http.MethodPut && r.URL.Path == "/library/sections/1/all":
			f.mu.Lock()
			f.edits = append(f.edits, r.URL.RawQuery)
			f.mu.Unlock()
		case r.URL.Path == "/library/metadata/777/children":
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []any{
				map[string]any{"ratingKey": "801", "index": 1, "thumb": "/library/metadata/801/thumb/1"},
				map[string]any{"ratingKey": "802", "index": 2, "thumb": "/library/metadata/802/thumb/1"}}}})
		case r.URL.Path == "/library/metadata/777":
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []any{map[string]any{
				"title": "1923", "thumb": "/library/metadata/777/thumb/9", "art": "/library/metadata/777/art/9",
				"Field": []any{map[string]any{"name": "thumb", "locked": true}},
				"Label": []any{map[string]any{"tag": "Overlay"}}}}}})
		case r.URL.Path == "/photo/:/transcode":
			if !strings.HasPrefix(r.URL.Query().Get("url"), "/library/metadata/") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG fake"))
		case r.URL.Path == "/library/metadata/777/allLeaves":
			_ = json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []any{
				map[string]any{"ratingKey": "901"}, map[string]any{"ratingKey": "902"}, map[string]any{"ratingKey": "903"}}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWatcherRefreshesPlexAndPostsToDiscordOnNewArtwork(t *testing.T) {
	fp := &fakePlex{}
	plexSrv := fp.server(t)
	var posts atomic.Int32
	var lastPost string
	var postMu sync.Mutex
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		postMu.Lock()
		lastPost = string(raw)
		postMu.Unlock()
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)

	h := newHarness(t, func(s *settings) {
		s.PlexServers = []plexServer{{Name: "Unlimited-Primary", URL: plexSrv.URL, Token: "plex-token"}}
		s.DiscordWebhook = "https://discord.com/api/webhooks/1/abc"
	})
	// Plex scans the show: the watcher records what it was given.
	metadata(t, h.post(t, "/tv/library/metadata/matches", map[string]any{"type": 2, "title": "1923", "includeChildren": 1}))

	ctx := context.Background()
	if n := h.app.watch.checkDue(ctx, true); n != 0 {
		t.Fatalf("nothing changed on Mediux yet, but %d titles were flagged", n)
	}
	// A creator uploads the S2E3 title card.
	extraCard.Store(true)
	if n := h.app.watch.checkDue(ctx, true); n != 1 {
		t.Fatalf("want 1 title with new artwork, got %d", n)
	}
	if len(fp.refreshed) != 1 || fp.refreshed[0] != "/library/metadata/777/refresh" {
		t.Fatalf("Plex refreshes: %v", fp.refreshed)
	}
	// Send the queued Discord message to the test webhook.
	if err := h.app.discord.send(ctx, hook.URL, h.app.discord.queue); err != nil {
		t.Fatal(err)
	}
	postMu.Lock()
	if !strings.Contains(lastPost, "S2E3") || !strings.Contains(lastPost, "Unlimited-Primary") {
		t.Errorf("Discord message should name the new card and the server: %s", lastPost)
	}
	postMu.Unlock()

	// Changing your own creator order is not "new artwork": no refresh, no message.
	s := h.store.get()
	s.PriorityCreators = []string{"Someone"}
	if err := h.store.save(s); err != nil {
		t.Fatal(err)
	}
	if n := h.app.watch.checkDue(ctx, true); n != 0 {
		t.Fatalf("a settings change must not count as new artwork, got %d", n)
	}
	if len(fp.refreshed) != 1 {
		t.Fatalf("no extra Plex refreshes expected: %v", fp.refreshed)
	}
}

func TestPlexTokensAndWebhookAreKeptAndHidden(t *testing.T) {
	h := newHarness(t, func(s *settings) {
		s.PlexServers = []plexServer{{Name: "Main", URL: "http://10.0.0.5:32400", Token: "secret-plex"}}
		s.DiscordWebhook = "https://discord.com/api/webhooks/1/abc"
	})
	got := h.get(t, "/api/settings")
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "secret-plex") || strings.Contains(string(raw), "webhooks/1/abc") {
		t.Fatalf("secrets leaked to the page: %s", raw)
	}
	if got["discord_configured"] != true || got["plex_servers"].([]any)[0].(map[string]any)["has_token"] != true {
		t.Fatalf("page should know secrets are set: %s", raw)
	}
	// Save the page's view back (blank secrets) and check they are kept.
	got["plex_servers"] = []any{map[string]any{"name": "Main", "url": "http://10.0.0.5:32400/"}}
	body, _ := json.Marshal(got)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/settings", strings.NewReader(string(body)))
	req.Header.Set("X-Mediux-Settings", "1")
	decode(t, mustDo(t, req))
	s := h.store.get()
	if s.PlexServers[0].Token != "secret-plex" || s.DiscordWebhook == "" || s.Token != "good-token" {
		t.Fatalf("secrets not kept: %+v", s)
	}
	// Correcting the address with the token left blank keeps the token too.
	got["plex_servers"] = []any{map[string]any{"name": "Main", "url": "http://10.0.0.5:32401"}}
	body, _ = json.Marshal(got)
	req, _ = http.NewRequest(http.MethodPost, h.server.URL+"/api/settings", strings.NewReader(string(body)))
	req.Header.Set("X-Mediux-Settings", "1")
	decode(t, mustDo(t, req))
	if s := h.store.get(); s.PlexServers[0].Token != "secret-plex" || s.PlexServers[0].URL != "http://10.0.0.5:32401" {
		t.Fatalf("token lost when the address changed: %+v", s.PlexServers)
	}
}

func TestDescribeSlots(t *testing.T) {
	got := describeSlots([]string{"poster", "s2", "s2e5", "s2e6"})
	if got != "poster; season posters 2; title cards S2E5, S2E6" {
		t.Errorf("got %q", got)
	}
}

func savePick(t *testing.T, h *harness, body map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/picks", strings.NewReader(string(raw)))
	req.Header.Set("X-Mediux-Settings", "1")
	return decode(t, mustDo(t, req))
}

func TestPagePickBeatsAuraAndCreatorPriority(t *testing.T) {
	path := t.TempDir() + "/AURA.db"
	writeAuraDB(t, path)
	h := newHarness(t, func(s *settings) {
		s.AuraDB = path
		s.TitlecardConsistency, s.MissingArtwork = modeMostComplete, "KeepExisting"
		s.SeasonConsistency = modeCreatorPriority // rules alone would say willtong93's set
	})
	// AURA pinned set-a's poster; pick MrMagixx's set-c on the page. He is
	// excluded, but a set you pick yourself still applies.
	savePick(t, h, map[string]any{"type": "movie", "tmdb": "291870", "title": "Dirty Grandpa", "poster_set": "set-c"})
	movie := metadata(t, h.get(t, "/movies/library/metadata/tmdb-291870"))[0]
	if assetOf(movie["thumb"]) != uuid(6) {
		t.Errorf("page pick should win over AURA: %v", movie["thumb"])
	}

	// Title cards: the rules would take the complete set, AURA pinned S1E1
	// from it too; the page pick is willtong93's partial set.
	savePick(t, h, map[string]any{"type": "show", "tmdb": "194764", "title": "1923", "titlecard_set": "favorite"})
	s1 := metadata(t, h.get(t, "/tv/library/metadata/tmdb-194764-s1?includeChildren=1"))[0]
	eps := s1["Children"].(map[string]any)["Metadata"].([]any)
	if got := assetOf(eps[0].(map[string]any)["thumb"]); got != uuid(22) {
		t.Errorf("S1E1 should come from the picked set, got %v", got)
	}
	// Season 2 isn't in the picked set: it follows the rules (complete set).
	s2 := metadata(t, h.get(t, "/tv/library/metadata/tmdb-194764-s2?includeChildren=1"))[0]
	e := s2["Children"].(map[string]any)["Metadata"].([]any)[0].(map[string]any)
	if assetOf(e["thumb"]) != uuid(16) {
		t.Errorf("S2E1 should fall back to the rules, got %v", e["thumb"])
	}

	// The Sets tab lists every set and shows the saved pick.
	sets := h.get(t, "/api/sets?type=show&q=194764")
	if n := len(sets["sets"].([]any)); n != 2 {
		t.Errorf("want 2 sets listed, got %d", n)
	}
	if sets["pick"].(map[string]any)["titlecard_set"] != "favorite" {
		t.Errorf("saved pick not reported: %v", sets["pick"])
	}

	// With AURA pinning the S1 season poster from the complete set, the page
	// reports that set as the one in use for season posters.
	if auto := sets["auto"].(map[string]any); auto["season_set"] != "complete" {
		t.Errorf("AURA's season set should show as in use: %v", auto)
	}

	// Clearing every part removes the pick.
	savePick(t, h, map[string]any{"type": "show", "tmdb": "194764"})
	if items := h.get(t, "/api/picks")["items"].([]any); len(items) != 1 {
		t.Errorf("only the movie pick should remain, got %v", items)
	}
}

func TestSavingAPickUnlocksAndRefreshesPlex(t *testing.T) {
	fp := &fakePlex{}
	plexSrv := fp.server(t)
	h := newHarness(t, func(s *settings) {
		s.PlexServers = []plexServer{{Name: "Unlimited-Primary", URL: plexSrv.URL, Token: "plex-token"}}
		s.ReplaceLocked = true
	})
	savePick(t, h, map[string]any{"type": "show", "tmdb": "194764", "title": "1923", "season_set": "favorite"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		fp.mu.Lock()
		done := len(fp.refreshed) > 0
		fp.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	fp.mu.Lock()
	refreshed, edits := append([]string(nil), fp.refreshed...), append([]string(nil), fp.edits...)
	fp.edits = nil
	fp.mu.Unlock()
	if len(refreshed) != 1 || refreshed[0] != "/library/metadata/777/refresh" {
		t.Fatalf("refreshes: %v", refreshed)
	}
	all := strings.Join(edits, "\n")
	for _, want := range []string{"type=2", "id=777", "thumb.locked=0", "label%5B%5D.tag.tag-=Overlay",
		"type=3", "id=801%2C802", "type=4", "id=901%2C902%2C903"} {
		if !strings.Contains(all, want) {
			t.Errorf("unlock edits missing %q in:\n%s", want, all)
		}
	}
	// Without the option, nothing is unlocked.
	s := h.store.get()
	s.ReplaceLocked = false
	_ = h.store.save(s)
	if _, err := h.app.plexAPI.refresh(context.Background(), s.PlexServers, typeShow, "194764", false); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	edits = append([]string(nil), fp.edits...)
	fp.mu.Unlock()
	if len(edits) != 0 {
		t.Errorf("nothing should be unlocked without the option: %v", edits)
	}
}

func TestPlexNowShowsCurrentArtworkThroughTheContainer(t *testing.T) {
	fp := &fakePlex{}
	plexSrv := fp.server(t)
	h := newHarness(t, func(s *settings) {
		s.PlexServers = []plexServer{{Name: "Unlimited-Primary", URL: plexSrv.URL, Token: "plex-token"}}
	})
	out := h.get(t, "/api/plex-current?type=show&tmdb=194764")
	servers := out["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("want one server, got %v", out)
	}
	cur := servers[0].(map[string]any)
	if cur["locked"] != true || cur["overlay"] != true || len(cur["seasons"].([]any)) != 2 {
		t.Fatalf("current artwork: %v", cur)
	}
	poster := cur["poster"].(string)
	if !strings.HasPrefix(poster, "/api/plex-image?") || strings.Contains(poster, "plex-token") {
		t.Fatalf("poster should go through the container without the token: %s", poster)
	}
	res, err := http.Get(h.server.URL + poster)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("relayed image: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	// Only Plex library artwork can be relayed.
	res, err = http.Get(h.server.URL + "/api/plex-image?s=0&w=100&h=100&p=%2Fsystem%2Fsettings")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-library path should be refused, got %d", res.StatusCode)
	}
}

func TestCollectionPickCoversEveryMovie(t *testing.T) {
	h := newHarness(t, nil)
	// An existing pick for one movie keeps its other parts.
	if err := h.app.provider.picks.put(userPick{Type: typeMovie, TMDB: "555", Title: "Dirty Grandpa 2", BackSet: "set-a"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"set_id": "77", "poster": true, "background": true})
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/picks/collection", strings.NewReader(string(raw)))
	req.Header.Set("X-Mediux-Settings", "1")
	out := decode(t, mustDo(t, req))
	if !strings.Contains(out["status"].(string), "2 movies") {
		t.Fatalf("status: %v", out)
	}
	store := h.app.provider.picks
	if p := store.get(typeMovie, "291870"); p == nil || p.PosterSet != "77" || p.BackSet != "77" {
		t.Fatalf("Dirty Grandpa pick: %+v", p)
	}
	// The second movie has no collection background, so its own stays.
	if p := store.get(typeMovie, "555"); p == nil || p.PosterSet != "77" || p.BackSet != "set-a" {
		t.Fatalf("Dirty Grandpa 2 pick: %+v", p)
	}
}
