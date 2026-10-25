package main

// Plex custom metadata provider endpoints. Two providers are served because
// Plex only combines providers that support the same types:
//   /movies  movies (type 1)
//   /tv      shows, seasons and episodes (types 2, 3, 4)
// Responses carry artwork only. Plex Movie / Plex Series stay Primary and
// supply titles, dates, summaries and matching.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	movieIdentifier = "tv.plex.agents.custom.mediux.jjbobzin.movies"
	tvIdentifier    = "tv.plex.agents.custom.mediux.jjbobzin.tv"
)

var (
	movieKeyRe = regexp.MustCompile(`^tmdb-(\d+)$`)
	tvKeyRe    = regexp.MustCompile(`^tmdb-(\d+)(?:-s(\d+)(?:e(\d+))?)?$`)
)

type plexProvider struct {
	mediux   *mediuxClient
	ids      *identifier
	settings *settingsStore
	activity *activityLog
	aura     *auraStore
	picks    *pickStore
	watch    *watcher
}

// Reasons a title gets no Mediux art from Plex's point of view.
const (
	reasonReview   = "waiting for review"
	reasonKeepPlex = "kept Plex art"
)

// gate says whether Plex may get Mediux art for a title: "" if yes, else why
// not. "Keep Plex art" always wins; in "approved" mode a title also needs a
// pick made on the page or in AURA.
func (p *plexProvider) gate(typ mediaType, tmdb string) string {
	pick := p.picks.get(typ, tmdb)
	if pick != nil && pick.KeepPlex {
		return reasonKeepPlex
	}
	if p.settings.get().ApplyMode == "all" {
		return ""
	}
	if pick != nil && pick.hasSets() || p.auraFor(typ, tmdb) != nil {
		return ""
	}
	return reasonReview
}

// overrides gathers the settings-page pick and the AURA pick for a title.
func (p *plexProvider) overrides(typ mediaType, tmdb string) override {
	return override{pick: p.picks.get(typ, tmdb), aura: p.auraFor(typ, tmdb)}
}

// auraFor is the title's AURA pick, unless you chose to ignore that pick here.
func (p *plexProvider) auraFor(typ mediaType, tmdb string) *auraPin {
	pin := p.aura.pin(typ, tmdb)
	if pin == nil {
		return nil
	}
	if pick := p.picks.get(typ, tmdb); pick != nil && pick.AuraIgnored == pin.signature() {
		return nil
	}
	return pin
}

type plexRequest struct {
	w        http.ResponseWriter
	r        *http.Request
	language string
	base     string // this server's address as Plex sees it, for relayed images
}

func newPlexRequest(w http.ResponseWriter, r *http.Request) *plexRequest {
	lang := r.Header.Get("X-Plex-Language")
	if lang == "" {
		lang = r.URL.Query().Get("X-Plex-Language")
	}
	return &plexRequest{w: w, r: r, language: plexLanguage(lang), base: "http://" + r.Host}
}

func (pr *plexRequest) param(name, def string) string {
	if v := pr.r.Header.Get(name); v != "" {
		return v
	}
	if v := pr.r.URL.Query().Get(name); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func mediaContainer(identifier, field string, entries any, size, total, offset int) map[string]any {
	return map[string]any{"MediaContainer": map[string]any{
		"offset": offset, "totalSize": total, "identifier": identifier, "size": size, field: entries,
	}}
}

func providerDefinition(identifier, title string, types []int) map[string]any {
	var ts []map[string]any
	for _, t := range types {
		ts = append(ts, map[string]any{"type": t, "Scheme": []map[string]string{{"scheme": identifier}}})
	}
	return map[string]any{"MediaProvider": map[string]any{
		"identifier": identifier, "title": title, "version": version, "Types": ts,
		"Feature": []map[string]string{
			{"type": "metadata", "key": "/library/metadata"},
			{"type": "match", "key": "/library/metadata/matches"},
		},
	}}
}

// ServeHTTP routes /movies/... and /tv/... requests.
func (p *plexProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pr := newPlexRequest(w, r)
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case path == "/movies":
		writeJSON(w, 200, providerDefinition(movieIdentifier, "Mediux Artwork (Movies)", []int{1}))
	case path == "/tv":
		writeJSON(w, 200, providerDefinition(tvIdentifier, "Mediux Artwork (TV)", []int{2, 3, 4}))
	case path == "/movies/library/metadata/matches" && r.Method == http.MethodPost:
		p.movieMatch(pr)
	case path == "/tv/library/metadata/matches" && r.Method == http.MethodPost:
		p.tvMatch(pr)
	case strings.HasPrefix(path, "/movies/library/metadata/"):
		p.movieMetadata(pr, strings.TrimPrefix(path, "/movies/library/metadata/"))
	case strings.HasPrefix(path, "/tv/library/metadata/"):
		p.tvMetadata(pr, strings.TrimPrefix(path, "/tv/library/metadata/"))
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

func readHints(r *http.Request) hints {
	var h hints
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &h)
	}
	q := r.URL.Query()
	if h.Type == nil && q.Get("type") != "" { // hints may arrive as query parameters
		h.Type = q.Get("type")
		h.Title, h.ParentTitle, h.GrandparentTitle = q.Get("title"), q.Get("parentTitle"), q.Get("grandparentTitle")
		h.Guid, h.Filename, h.IncludeChildren, h.Manual = q.Get("guid"), q.Get("filename"), q.Get("includeChildren"), q.Get("manual")
		h.Year, _ = strconv.Atoi(q.Get("year"))
		if v, err := strconv.Atoi(q.Get("index")); err == nil {
			h.Index = &v
		}
		if v, err := strconv.Atoi(q.Get("parentIndex")); err == nil {
			h.ParentIndex = &v
		}
	}
	return h
}

func (p *plexProvider) ctx(pr *plexRequest) (context.Context, context.CancelFunc) {
	return context.WithTimeout(pr.r.Context(), 45*time.Second)
}

// fail records an error. Without a token yet, Plex gets an empty answer
// rather than an error, so scans carry on with Plex's own artwork.
func (p *plexProvider) fail(pr *plexRequest, identifier, kind, label string, err error) {
	if label != "" {
		p.activity.add(kind, label, "error", err.Error())
	}
	if errors.Is(err, errNoToken) {
		writeJSON(pr.w, 200, mediaContainer(identifier, "Metadata", []any{}, 0, 0, 0))
		return
	}
	writeJSON(pr.w, http.StatusBadGateway, map[string]string{"error": err.Error()})
}

// ---- images ----------------------------------------------------------

func (p *plexProvider) url(pr *plexRequest, a artwork, cfg settings) string {
	u := imageURL(a, cfg.Quality)
	if cfg.ImageDelivery == "relay" {
		return pr.base + "/img/" + strings.TrimPrefix(u, imageHost)
	}
	return u
}

type plexImage struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	Alt  string `json:"alt,omitempty"`
}

// ---- movies ------------------------------------------------------------

func (p *plexProvider) movieMatch(pr *plexRequest) {
	h := readHints(pr.r)
	if typeNumber(h.Type) != 1 {
		writeJSON(pr.w, 200, mediaContainer(movieIdentifier, "Metadata", []any{}, 0, 0, 0))
		return
	}
	ctx, cancel := p.ctx(pr)
	defer cancel()
	label := h.Title
	if h.Year > 0 {
		label = fmt.Sprintf("%s (%d)", h.Title, h.Year)
	}
	id, how, err := p.ids.movie(ctx, h)
	if err != nil {
		p.fail(pr, movieIdentifier, "movie", label, err)
		return
	}
	if id == "" {
		p.activity.add("movie", label, "not found", "no TMDB ID in the file name and no exact title match on Mediux")
		writeJSON(pr.w, 200, mediaContainer(movieIdentifier, "Metadata", []any{}, 0, 0, 0))
		return
	}
	item, found, err := p.movieItem(ctx, pr, id, label)
	if err != nil {
		p.fail(pr, movieIdentifier, "movie", label, err)
		return
	}
	p.activity.add("movie", label, resultWord(found), fmt.Sprintf("TMDB %s via %s; %s", id, how, found))
	writeJSON(pr.w, 200, mediaContainer(movieIdentifier, "Metadata", []any{item}, 1, 1, 0))
}

// describe names an image and who it came from, for recent activity.
func describe(what string, c choice) string {
	switch c.source {
	case "pick":
		return what + " (your pick)"
	case "aura":
		return what + " (AURA pick)"
	}
	return what + " by " + c.art.Creator
}

func resultWord(found string) string {
	switch found {
	case "no Mediux artwork":
		return "no artwork"
	case reasonReview:
		return "to review"
	case reasonKeepPlex:
		return "kept Plex art"
	}
	return "matched"
}

func (p *plexProvider) movieMetadata(pr *plexRequest, rest string) {
	key, sub, _ := strings.Cut(rest, "/")
	m := movieKeyRe.FindStringSubmatch(key)
	if m == nil {
		writeJSON(pr.w, 404, map[string]string{"error": "unknown item"})
		return
	}
	ctx, cancel := p.ctx(pr)
	defer cancel()
	switch sub {
	case "":
		item, found, err := p.movieItem(ctx, pr, m[1], "TMDB "+m[1])
		if err != nil {
			p.fail(pr, movieIdentifier, "movie", "TMDB "+m[1], err)
			return
		}
		p.activity.add("movie", "TMDB "+m[1], resultWord(found), "refresh; "+found)
		writeJSON(pr.w, 200, mediaContainer(movieIdentifier, "Metadata", []any{item}, 1, 1, 0))
	case "images":
		images, err := p.movieGallery(ctx, pr, m[1])
		if err != nil {
			p.fail(pr, movieIdentifier, "movie", "", err)
			return
		}
		writeJSON(pr.w, 200, mediaContainer(movieIdentifier, "Image", images, len(images), len(images), 0))
	case "extras":
		writeJSON(pr.w, 200, mediaContainer(movieIdentifier, "Metadata", []any{}, 0, 0, 0))
	default:
		writeJSON(pr.w, 404, map[string]string{"error": "not found"})
	}
}

// movieItem builds the artwork-only metadata item for a movie.
func (p *plexProvider) movieItem(ctx context.Context, pr *plexRequest, tmdb, label string) (map[string]any, string, error) {
	key := "tmdb-" + tmdb
	item := map[string]any{
		"ratingKey": key, "key": "/library/metadata/" + key,
		"guid": movieIdentifier + "://movie/" + key, "type": "movie",
	}
	b, err := p.mediux.artwork(ctx, typeMovie, tmdb)
	if err != nil {
		return nil, "", err
	}
	cfg := p.settings.get()
	ov := p.overrides(typeMovie, tmdb)
	blocked := p.gate(typeMovie, tmdb)
	if blocked != "" {
		b, ov = bundle{}, override{} // Plex keeps its own artwork
	}
	var images []plexImage
	var parts []string
	poster := choosePoster(b, cfg, pr.language, ov)
	if poster.ok {
		item["thumb"] = p.url(pr, poster.art, cfg)
		images = append(images, plexImage{"coverPoster", item["thumb"].(string), poster.art.Creator})
		parts = append(parts, describe("poster", poster))
	}
	if c := chooseWithPoster(b, cfg, kindBackdrop, pr.language, poster, ov); c.ok {
		item["art"] = p.url(pr, c.art, cfg)
		images = append(images, plexImage{"background", item["art"].(string), c.art.Creator})
		parts = append(parts, describe("background", c))
	}
	if c := chooseWithPoster(b, cfg, kindLogo, pr.language, poster, ov); c.ok {
		images = append(images, plexImage{"clearLogo", p.url(pr, c.art, cfg), c.art.Creator})
		parts = append(parts, "logo")
	}
	if len(images) > 0 {
		item["Image"] = images
	}
	summary := "no Mediux artwork"
	if blocked != "" {
		summary = blocked
	} else if len(parts) > 0 {
		summary = strings.Join(parts, ", ")
	}
	posterURL := ""
	if poster.ok {
		posterURL = imageURL(poster.art, "optimized")
	}
	p.watch.observe(typeMovie, tmdb, label, pr.language, movieSignature(b, cfg, pr.language, ov), basis(cfg, ov), posterURL, summary)
	return item, summary, nil
}

// movieGallery lists every eligible image for Plex's Edit → Poster picker.
func (p *plexProvider) movieGallery(ctx context.Context, pr *plexRequest, tmdb string) ([]plexImage, error) {
	b, err := p.mediux.artwork(ctx, typeMovie, tmdb)
	if err != nil {
		return nil, err
	}
	cfg := p.settings.get()
	images := []plexImage{}
	for _, k := range []struct{ kind, plex string }{{kindPoster, "coverPoster"}, {kindBackdrop, "background"}, {kindLogo, "clearLogo"}} {
		for _, a := range candidates(b, cfg, k.kind, 0, 0) {
			images = append(images, plexImage{k.plex, p.url(pr, a, cfg), a.Creator})
		}
	}
	return images, nil
}

// ---- TV ------------------------------------------------------------------

func (p *plexProvider) tvMatch(pr *plexRequest) {
	h := readHints(pr.r)
	ctx, cancel := p.ctx(pr)
	defer cancel()
	var title string
	t := typeNumber(h.Type)
	switch t {
	case 2:
		title = h.Title
	case 3:
		title = h.ParentTitle
	case 4:
		title = h.GrandparentTitle
	default:
		writeJSON(pr.w, 200, mediaContainer(tvIdentifier, "Metadata", []any{}, 0, 0, 0))
		return
	}
	if t == 3 && h.Index == nil || t == 4 && (h.Index == nil || h.ParentIndex == nil) {
		writeJSON(pr.w, 200, mediaContainer(tvIdentifier, "Metadata", []any{}, 0, 0, 0))
		return
	}
	id, how, err := p.ids.show(ctx, title, h)
	if err != nil {
		p.fail(pr, tvIdentifier, "show", title, err)
		return
	}
	if id == "" {
		if t == 2 {
			p.activity.add("show", title, "not found", "no ID in the file name and no exact title match")
		}
		writeJSON(pr.w, 200, mediaContainer(tvIdentifier, "Metadata", []any{}, 0, 0, 0))
		return
	}
	sh, err := p.loadShow(ctx, pr, id)
	if err != nil {
		p.fail(pr, tvIdentifier, "show", title, err)
		return
	}
	sh.block(p.gate(typeShow, id))
	var item map[string]any
	switch t {
	case 2:
		item = sh.showItem()
		if isOn(h.IncludeChildren) {
			sh.attachChildren(item, -1)
		}
		summary := sh.summary()
		p.activity.add("show", title, resultWord(summary), fmt.Sprintf("TMDB %s via %s; %s", id, how, summary))
		sh.observe(title, summary)
	case 3:
		item = sh.seasonItem(*h.Index)
		if isOn(h.IncludeChildren) {
			sh.attachChildren(item, *h.Index)
		}
	case 4:
		item = sh.episodeItem(*h.ParentIndex, *h.Index)
	}
	writeJSON(pr.w, 200, mediaContainer(tvIdentifier, "Metadata", []any{item}, 1, 1, 0))
}

func (p *plexProvider) tvMetadata(pr *plexRequest, rest string) {
	key, sub, _ := strings.Cut(rest, "/")
	m := tvKeyRe.FindStringSubmatch(key)
	if m == nil {
		writeJSON(pr.w, 404, map[string]string{"error": "unknown item"})
		return
	}
	season, episode := -1, -1
	if m[2] != "" {
		season, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		episode, _ = strconv.Atoi(m[3])
	}
	ctx, cancel := p.ctx(pr)
	defer cancel()
	if sub == "extras" {
		writeJSON(pr.w, 200, mediaContainer(tvIdentifier, "Metadata", []any{}, 0, 0, 0))
		return
	}
	sh, err := p.loadShow(ctx, pr, m[1])
	if err != nil {
		p.fail(pr, tvIdentifier, "show", "TMDB "+m[1], err)
		return
	}
	if sub != "images" { // the Edit → Poster picker is your own manual choice
		sh.block(p.gate(typeShow, m[1]))
	}
	item := sh.showItem()
	if episode >= 0 {
		item = sh.episodeItem(season, episode)
	} else if season >= 0 {
		item = sh.seasonItem(season)
	}
	switch sub {
	case "":
		if pr.param("includeChildren", "") == "1" && episode < 0 {
			sh.attachChildren(item, season)
		}
		if season < 0 {
			summary := sh.summary()
			p.activity.add("show", "TMDB "+m[1], resultWord(summary), "refresh; "+summary)
			sh.observe("TMDB "+m[1], summary)
		}
		writeJSON(pr.w, 200, mediaContainer(tvIdentifier, "Metadata", []any{item}, 1, 1, 0))
	case "images":
		images := sh.gallery(season, episode)
		writeJSON(pr.w, 200, mediaContainer(tvIdentifier, "Image", images, len(images), len(images), 0))
	case "children", "grandchildren":
		var kids []map[string]any
		if sub == "children" && episode < 0 {
			kids = sh.children(season)
		} else if sub == "grandchildren" && season < 0 {
			for _, s := range sh.seasonNumbers() {
				kids = append(kids, sh.children(s)...)
			}
		}
		start, _ := strconv.Atoi(pr.param("X-Plex-Container-Start", "0"))
		size, _ := strconv.Atoi(pr.param("X-Plex-Container-Size", "20"))
		start = max(start, 0)
		if size <= 0 {
			size = 20
		}
		total := len(kids)
		page := []map[string]any{}
		if start < total {
			page = kids[start:min(total, start+size)]
		}
		writeJSON(pr.w, 200, mediaContainer(tvIdentifier, "Metadata", page, len(page), total, start))
	default:
		writeJSON(pr.w, 404, map[string]string{"error": "not found"})
	}
}

// show holds one show's artwork choices for a single request.
type show struct {
	p         *plexProvider
	pr        *plexRequest
	cfg       settings
	b         bundle
	tmdb      string
	seasons   setChoice
	cards     setChoice
	ov        override
	blocked   string        // why Plex gets no Mediux art for this show, if it doesn't
	structure map[int][]int // season -> episodes, from Mediux and artwork coverage
}

func (p *plexProvider) loadShow(ctx context.Context, pr *plexRequest, tmdb string) (*show, error) {
	b, err := p.mediux.artwork(ctx, typeShow, tmdb)
	if err != nil {
		return nil, err
	}
	cfg := p.settings.get()
	sh := &show{p: p, pr: pr, cfg: cfg, b: b, tmdb: tmdb,
		seasons:   chooseSet(b, cfg, kindSeason, pr.language),
		cards:     chooseSet(b, cfg, kindStill, pr.language),
		structure: map[int][]int{}}
	sh.ov = p.overrides(typeShow, tmdb)
	add := func(s, e int) {
		if _, ok := sh.structure[s]; !ok {
			sh.structure[s] = nil
		}
		if e > 0 && !containsInt(sh.structure[s], e) {
			sh.structure[s] = append(sh.structure[s], e)
		}
	}
	if st, err := p.mediux.showStructure(ctx, tmdb); err == nil {
		for s, eps := range st {
			add(s, 0)
			for _, e := range eps {
				add(s, e)
			}
		}
	}
	for _, s := range b.Sets {
		for _, a := range s.Art {
			switch a.Kind {
			case kindSeason:
				add(a.Season, 0)
			case kindStill:
				add(a.Season, a.Episode)
			}
		}
	}
	if pin := sh.ov.aura; pin != nil {
		for season := range pin.Seasons {
			add(season, 0)
		}
		for se := range pin.Cards {
			add(se[0], se[1])
		}
	}
	for s := range sh.structure {
		sort.Ints(sh.structure[s])
	}
	return sh, nil
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (sh *show) keys(season, episode int) (key, guid string) {
	key = "tmdb-" + sh.tmdb
	kind := "show"
	if season >= 0 {
		key += fmt.Sprintf("-s%d", season)
		kind = "season"
	}
	if episode >= 0 {
		key += fmt.Sprintf("e%d", episode)
		kind = "episode"
	}
	return key, tvIdentifier + "://" + kind + "/" + key
}

// observe tells the watcher what Plex was just given for this show.
func (sh *show) observe(title, summary string) {
	posterURL := ""
	if c := sh.poster(); c.ok {
		posterURL = imageURL(c.art, "optimized")
	}
	sh.p.watch.observe(typeShow, sh.tmdb, title, sh.pr.language, sh.signature(), basis(sh.cfg, sh.ov), posterURL, summary)
}

// block withholds all Mediux art from Plex for this show, for the reason given.
func (sh *show) block(reason string) {
	if reason == "" {
		return
	}
	sh.blocked = reason
	sh.b, sh.ov, sh.seasons, sh.cards = bundle{}, override{}, setChoice{}, setChoice{}
}

func (sh *show) poster() choice {
	return choosePoster(sh.b, sh.cfg, sh.pr.language, sh.ov)
}

func (sh *show) backdrop() choice {
	return chooseWithPoster(sh.b, sh.cfg, kindBackdrop, sh.pr.language, sh.poster(), sh.ov)
}

func (sh *show) logo() choice {
	return chooseWithPoster(sh.b, sh.cfg, kindLogo, sh.pr.language, sh.poster(), sh.ov)
}

func (sh *show) season(season int) choice {
	return chooseEach(sh.b, sh.cfg, kindSeason, season, 0, sh.pr.language, sh.seasons, sh.ov)
}

func (sh *show) card(season, episode int) choice {
	return chooseEach(sh.b, sh.cfg, kindStill, season, episode, sh.pr.language, sh.cards, sh.ov)
}

func (sh *show) summary() string {
	if sh.blocked != "" {
		return sh.blocked
	}
	var parts []string
	if c := sh.poster(); c.ok {
		parts = append(parts, describe("poster", c))
	}
	seasons, cards := 0, 0
	for s, eps := range sh.structure {
		if sh.season(s).ok {
			seasons++
		}
		for _, e := range eps {
			if sh.card(s, e).ok {
				cards++
			}
		}
	}
	if seasons > 0 {
		parts = append(parts, plural(seasons, "season poster", "season posters"))
	}
	if cards > 0 {
		parts = append(parts, plural(cards, "title card", "title cards"))
	}
	if len(parts) == 0 {
		return "no Mediux artwork"
	}
	if sh.ov.pick != nil {
		parts = append(parts, "your picks applied")
	} else if sh.ov.aura != nil {
		parts = append(parts, "AURA picks applied")
	}
	return strings.Join(parts, ", ")
}

func (sh *show) showItem() map[string]any {
	key, guid := sh.keys(-1, -1)
	item := map[string]any{"ratingKey": key, "key": "/library/metadata/" + key, "guid": guid, "type": "show"}
	var images []plexImage
	if c := sh.poster(); c.ok {
		item["thumb"] = sh.p.url(sh.pr, c.art, sh.cfg)
		images = append(images, plexImage{"coverPoster", item["thumb"].(string), c.art.Creator})
	}
	if c := sh.backdrop(); c.ok {
		item["art"] = sh.p.url(sh.pr, c.art, sh.cfg)
		images = append(images, plexImage{"background", item["art"].(string), c.art.Creator})
	}
	if c := sh.logo(); c.ok {
		images = append(images, plexImage{"clearLogo", sh.p.url(sh.pr, c.art, sh.cfg), c.art.Creator})
	}
	if len(images) > 0 {
		item["Image"] = images
	}
	return item
}

func (sh *show) seasonItem(season int) map[string]any {
	key, guid := sh.keys(season, -1)
	showKey, showGuid := sh.keys(-1, -1)
	item := map[string]any{"ratingKey": key, "key": "/library/metadata/" + key, "guid": guid, "type": "season",
		"index": season, "parentRatingKey": showKey, "parentKey": "/library/metadata/" + showKey,
		"parentGuid": showGuid, "parentType": "show"}
	if c := sh.season(season); c.ok {
		item["thumb"] = sh.p.url(sh.pr, c.art, sh.cfg)
		item["Image"] = []plexImage{{"coverPoster", item["thumb"].(string), c.art.Creator}}
	}
	return item
}

func (sh *show) episodeItem(season, episode int) map[string]any {
	key, guid := sh.keys(season, episode)
	seasonKey, seasonGuid := sh.keys(season, -1)
	showKey, showGuid := sh.keys(-1, -1)
	item := map[string]any{"ratingKey": key, "key": "/library/metadata/" + key, "guid": guid, "type": "episode",
		"index": episode, "parentIndex": season,
		"parentRatingKey": seasonKey, "parentKey": "/library/metadata/" + seasonKey, "parentGuid": seasonGuid, "parentType": "season",
		"grandparentRatingKey": showKey, "grandparentKey": "/library/metadata/" + showKey, "grandparentGuid": showGuid, "grandparentType": "show"}
	if c := sh.card(season, episode); c.ok {
		item["thumb"] = sh.p.url(sh.pr, c.art, sh.cfg)
		item["Image"] = []plexImage{{"snapshot", item["thumb"].(string), c.art.Creator}}
	}
	return item
}

func (sh *show) seasonNumbers() []int {
	var out []int
	for s := range sh.structure {
		out = append(out, s)
	}
	sort.Ints(out)
	return out
}

// children lists seasons (season < 0) or one season's episodes.
func (sh *show) children(season int) []map[string]any {
	kids := []map[string]any{}
	if season < 0 {
		for _, s := range sh.seasonNumbers() {
			kids = append(kids, sh.seasonItem(s))
		}
		return kids
	}
	for _, e := range sh.structure[season] {
		kids = append(kids, sh.episodeItem(season, e))
	}
	return kids
}

func (sh *show) attachChildren(item map[string]any, season int) {
	kids := sh.children(season)
	item["Children"] = map[string]any{"size": len(kids), "Metadata": kids}
}

// gallery lists every eligible image for the picker.
func (sh *show) gallery(season, episode int) []plexImage {
	images := []plexImage{}
	add := func(kind, plexType string, s, e int) {
		for _, a := range candidates(sh.b, sh.cfg, kind, s, e) {
			images = append(images, plexImage{plexType, sh.p.url(sh.pr, a, sh.cfg), a.Creator})
		}
	}
	switch {
	case episode >= 0:
		add(kindStill, "snapshot", season, episode)
	case season >= 0:
		add(kindSeason, "coverPoster", season, 0)
	default:
		add(kindPoster, "coverPoster", 0, 0)
		add(kindBackdrop, "background", 0, 0)
		add(kindLogo, "clearLogo", 0, 0)
	}
	return images
}
