package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// uuid builds a valid Mediux-style asset ID from a small number.
func uuid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func asset(n int) map[string]any {
	return map[string]any{"id": uuid(n), "modified_on": "2026-01-02T03:04:05Z", "language": nil}
}

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
			map[string]any{"id": "favourite", "popularity": 1, "user_created": map[string]any{"username": "willtong93"},
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
			data = map[string]any{"shows_by_id": inner}
		case strings.Contains(body.Query, "shows(filter:{tvdb_id"):
			tmdbByTVDB := map[any]string{"416491": "194764", "71173": "501", "73545": "1972"}
			if id, ok := tmdbByTVDB[body.Variables["tvdb_id"]]; ok {
				data = map[string]any{"shows": []any{map[string]any{"id": id}}}
			} else {
				data = map[string]any{"shows": []any{}}
			}
		case strings.Contains(body.Query, "movies(limit:25"):
			data = map[string]any{"movies": []any{
				map[string]any{"id": "1", "title": "Dirty Grandpa Returns", "release_date": "2030-01-01"},
				map[string]any{"id": "291870", "title": "Dirty Grandpa", "release_date": "2016-01-22"},
			}}
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
	client := newMediuxClient(store)
	client.endpoint = mx.URL
	ids := newIdentifier(client)
	ids.tvmaze = tv.URL
	activity := &activityLog{}
	plex := &plexProvider{mediux: client, ids: ids, settings: store, activity: activity}
	ui := &webUI{settings: store, mediux: client, ids: ids, activity: activity, relay: http.DefaultClient}
	mux := http.NewServeMux()
	ui.routes(mux)
	for _, p := range []string{"/movies", "/movies/", "/tv", "/tv/"} {
		mux.Handle(p, plex)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{srv, store, activity}
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
