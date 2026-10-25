package main

// Settings page and its small JSON API, plus the optional image relay.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed web/settings.html
var settingsPage []byte

type webUI struct {
	settings     *settingsStore
	provider     *plexProvider
	plex         *plexClient
	discord      *discord
	auth         *plexAuth
	availability *availability
	updates      *updateChecker
	activity     *activityLog
	relay        *http.Client
	started      time.Time
}

func (ui *webUI) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", ui.page)
	mux.HandleFunc("GET /api/settings", ui.getSettings)
	mux.HandleFunc("POST /api/settings", ui.guard(ui.saveSettings))
	mux.HandleFunc("POST /api/check", ui.guard(ui.check))
	mux.HandleFunc("GET /api/status", ui.status)
	mux.HandleFunc("GET /api/version", ui.versionInfo)
	mux.HandleFunc("GET /api/creators", ui.creators)
	mux.HandleFunc("GET /api/activity", ui.recent)
	mux.HandleFunc("GET /api/preview", ui.preview)
	mux.HandleFunc("GET /api/aura", ui.auraPicks)
	mux.HandleFunc("POST /api/test-discord", ui.guard(ui.testDiscord))
	mux.HandleFunc("POST /api/test-plex", ui.guard(ui.testPlex))
	mux.HandleFunc("POST /api/check-now", ui.guard(ui.checkNow))
	mux.HandleFunc("GET /api/sets", ui.sets)
	mux.HandleFunc("GET /api/picks", ui.listPicks)
	mux.HandleFunc("POST /api/picks", ui.guard(ui.savePick))
	mux.HandleFunc("POST /api/picks/collection", ui.guard(ui.saveCollectionPick))
	mux.HandleFunc("GET /api/collection", ui.collection)
	mux.HandleFunc("GET /api/plex-current", ui.plexNow)
	mux.HandleFunc("GET /api/plex-image", ui.plexImage)
	mux.HandleFunc("GET /api/library", ui.library)
	mux.HandleFunc("GET /api/library/art", ui.libraryArt)
	mux.HandleFunc("POST /api/review/approve", ui.guard(ui.approve))
	mux.HandleFunc("POST /api/review/keep-plex", ui.guard(ui.keepPlex))
	mux.HandleFunc("GET /api/review/choices", ui.choices)
	mux.HandleFunc("POST /api/review/ignore-aura", ui.guard(ui.ignoreAura))
	mux.HandleFunc("POST /api/plex-auth/start", ui.guard(ui.plexAuthStart))
	mux.HandleFunc("GET /api/plex-auth/check", ui.plexAuthCheck)
	mux.HandleFunc("POST /api/plex-auth/add", ui.guard(ui.plexAuthAdd))
	mux.HandleFunc("GET /img/{id}", ui.relayImage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
}

func (ui *webUI) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https://images.mediux.io data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
	w.Write(settingsPage)
}

// guard blocks cross-site form posts: the page sends a custom header that
// other websites cannot add without permission.
func (ui *webUI) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Mediux-Settings") != "1" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing settings header"})
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request blocked"})
				return
			}
		}
		next(w, r)
	}
}

type publicServer struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	HasToken bool   `json:"has_token"`
}

// publicSettings is what the page sees: secrets are replaced by "is it set".
type publicSettings struct {
	settings
	Token             string         `json:"api_token,omitempty"`
	PlexServers       []publicServer `json:"plex_servers"`
	DiscordWebhook    string         `json:"discord_webhook,omitempty"`
	TokenConfigured   bool           `json:"token_configured"`
	DiscordConfigured bool           `json:"discord_configured"`
	Version           string         `json:"version"`
}

func (ui *webUI) getSettings(w http.ResponseWriter, _ *http.Request) {
	s := ui.settings.get()
	out := publicSettings{settings: s, PlexServers: []publicServer{},
		TokenConfigured: strings.TrimSpace(s.Token) != "", DiscordConfigured: s.DiscordWebhook != "", Version: version}
	for _, p := range s.PlexServers {
		out.PlexServers = append(out.PlexServers, publicServer{p.Name, p.URL, p.Token != ""})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}

func (ui *webUI) saveSettings(w http.ResponseWriter, r *http.Request) {
	current := ui.settings.get()
	// Start from the saved settings so fields the page doesn't send (an
	// older page left open, say) keep their values.
	in := struct {
		settings
		ClearToken   bool `json:"clear_token"`
		ClearDiscord bool `json:"clear_discord"`
	}{settings: current}
	in.Token, in.DiscordWebhook = "", "" // secrets only change when sent
	in.PlexServers = nil
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "could not read settings"})
		return
	}
	if in.PlexServers == nil {
		in.PlexServers = current.PlexServers
	}
	next := in.settings
	// Blank secrets mean "keep the saved one".
	next.Token = strings.TrimSpace(next.Token)
	if next.Token == "" && !in.ClearToken {
		next.Token = current.Token
	}
	next.DiscordWebhook = strings.TrimSpace(next.DiscordWebhook)
	if next.DiscordWebhook == "" && !in.ClearDiscord {
		next.DiscordWebhook = current.DiscordWebhook
	}
	// Match a blank token to the saved server by address, or by name when the
	// address was just corrected.
	byURL, byName := map[string]plexServer{}, map[string]plexServer{}
	for _, p := range current.PlexServers {
		byURL[strings.TrimRight(p.URL, "/")] = p
		byName[p.Name] = p
	}
	for i := range next.PlexServers {
		p := &next.PlexServers[i]
		p.Name, p.URL, p.Token = strings.TrimSpace(p.Name), strings.TrimRight(strings.TrimSpace(p.URL), "/"), strings.TrimSpace(p.Token)
		prev, ok := byURL[p.URL]
		if !ok {
			prev, ok = byName[p.Name]
		}
		p.Machine = "" // the page never sets this
		if ok {
			p.Machine = prev.Machine
			if p.Token == "" {
				p.Token = prev.Token
			}
		}
	}
	next.AuraDB = strings.TrimSpace(next.AuraDB)
	next.PlexClientID = current.PlexClientID
	if err := ui.settings.save(next); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	ui.provider.aura.refresh(true)
	ui.getSettings(w, r)
}

func (ui *webUI) check(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"api_token"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in)
	token := strings.TrimSpace(in.Token)
	if token == "" {
		token = ui.settings.get().Token
	}
	if strings.TrimSpace(token) == "" {
		writeJSON(w, 400, map[string]string{"error": "Enter your Mediux API token first."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := ui.provider.mediux.checkToken(ctx, token); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "Connected to Mediux."})
}

// status feeds the Overview tab.
func (ui *webUI) status(w http.ResponseWriter, _ *http.Request) {
	auraStatus, _ := ui.provider.aura.snapshot()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{
		"version":          version,
		"started":          ui.started,
		"token_configured": strings.TrimSpace(ui.settings.get().Token) != "",
		"totals":           ui.activity.totals(),
		"aura":             auraStatus,
		"watch":            ui.provider.watch.snapshot(),
	})
}

func (ui *webUI) creators(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if n := len([]rune(q)); n < 2 || n > 80 {
		writeJSON(w, 400, map[string]string{"error": "Type 2 to 80 characters."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	names, err := ui.provider.mediux.creators(ctx, q)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "Creator search failed. Check the saved Mediux token."})
		return
	}
	writeJSON(w, 200, map[string]any{"items": names})
}

func (ui *webUI) recent(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"items": ui.activity.recent()})
}

func (ui *webUI) auraPicks(w http.ResponseWriter, _ *http.Request) {
	st, pins := ui.provider.aura.snapshot()
	type row struct {
		auraPin
		Parts string `json:"parts"`
	}
	rows := []row{}
	for _, p := range pins {
		rows = append(rows, row{p, p.parts()})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"status": st, "items": rows})
}

func (ui *webUI) testDiscord(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Webhook string `json:"discord_webhook"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in)
	hook := strings.TrimSpace(in.Webhook)
	if hook == "" {
		hook = ui.settings.get().DiscordWebhook
	}
	if !discordWebhookRe.MatchString(hook) {
		writeJSON(w, 400, map[string]string{"error": "Paste a Discord webhook address first (https://discord.com/api/webhooks/…)."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	err := ui.discord.send(ctx, hook, []discordEmbed{{Title: "Mediux for Plex is connected",
		Description: "New Mediux artwork for your Plex libraries will be posted here.", Color: colorNew}})
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "Test message sent. Check the Discord channel."})
}

func (ui *webUI) testPlex(w http.ResponseWriter, r *http.Request) {
	var in plexServer
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in)
	in.URL = strings.TrimRight(strings.TrimSpace(in.URL), "/")
	in.Token = strings.TrimSpace(in.Token)
	if in.Token == "" {
		for _, p := range ui.settings.get().PlexServers {
			if strings.TrimRight(p.URL, "/") == in.URL {
				in.Token = p.Token
			}
		}
	}
	if u, err := url.Parse(in.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeJSON(w, 400, map[string]string{"error": "Enter the server address, like http://10.0.0.5:32400"})
		return
	}
	if in.Token == "" {
		writeJSON(w, 400, map[string]string{"error": "Enter the Plex token for this server."})
		return
	}
	if in.Name == "" {
		in.Name = in.URL
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	name, err := ui.plex.identify(ctx, in)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "Connected to " + name + "."})
}

// checkNow runs the watcher over every tracked title in the background.
func (ui *webUI) checkNow(w http.ResponseWriter, _ *http.Request) {
	st := ui.provider.watch.snapshot()
	if !st.Active {
		writeJSON(w, 400, map[string]string{"error": st.Reason})
		return
	}
	if st.Running {
		writeJSON(w, 409, map[string]string{"error": errWatchBusy.Error()})
		return
	}
	go ui.provider.watch.checkDue(context.Background(), true)
	writeJSON(w, 200, map[string]string{"status": "Checking every tracked title now. Results appear in the Log."})
}

type previewImage struct {
	Label   string `json:"label"`
	URL     string `json:"url"`
	Creator string `json:"creator"`
}

func previewOf(label string, c choice) previewImage {
	creator := c.art.Creator
	switch c.source {
	case "pick":
		creator += " · your pick"
	case "aura":
		creator += " · AURA pick"
	}
	return previewImage{label, imageURL(c.art, "optimized"), creator}
}

// resolve turns what was typed (a title, "Title (Year)" or a TMDB ID) into a
// TMDB ID. Titles come first: shows like "1923" or "24" are numbers, so a
// number with no exact title match is treated as a TMDB ID. A non-zero status
// means it failed, with msg to show.
func (ui *webUI) resolve(ctx context.Context, kind, q string) (tmdb, how string, status int, msg string) {
	if q == "" || (kind != "movie" && kind != "show") {
		return "", "", 400, "Enter a title or TMDB ID."
	}
	title, year := splitYear(q, 0)
	h := hints{Title: title, Year: year}
	var err error
	if kind == "movie" {
		tmdb, how, err = ui.provider.ids.movie(ctx, h)
	} else {
		tmdb, how, err = ui.provider.ids.show(ctx, q, h)
	}
	if err != nil {
		return "", "", 502, err.Error()
	}
	if tmdb == "" {
		if numericID(q) {
			return q, "TMDB ID", 0, ""
		}
		return "", "", 404, "No exact title match. Try the title with its year, like \"1923 (2022)\", or the TMDB ID."
	}
	return tmdb, how, 0, ""
}

// preview shows what would be chosen for a movie or show, without Plex.
func (ui *webUI) preview(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kind := r.URL.Query().Get("type")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tmdb, how, status, msg := ui.resolve(ctx, kind, q)
	if status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	cfg := ui.settings.get()
	images := []previewImage{}
	if kind == "movie" {
		b, err := ui.provider.mediux.artwork(ctx, typeMovie, tmdb)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		ov := ui.provider.overrides(typeMovie, tmdb)
		poster := choosePoster(b, cfg, "en", ov)
		if poster.ok {
			images = append(images, previewOf("Poster", poster))
		}
		if c := chooseWithPoster(b, cfg, kindBackdrop, "en", poster, ov); c.ok {
			images = append(images, previewOf("Background", c))
		}
		if c := chooseWithPoster(b, cfg, kindLogo, "en", poster, ov); c.ok {
			images = append(images, previewOf("Logo", c))
		}
	} else {
		sh, err := ui.provider.loadShow(ctx, &plexRequest{language: "en"}, tmdb)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		for _, x := range []struct {
			label string
			c     choice
		}{{"Poster", sh.poster()}, {"Background", sh.backdrop()}, {"Logo", sh.logo()}} {
			if x.c.ok {
				images = append(images, previewOf(x.label, x.c))
			}
		}
		for _, s := range sh.seasonNumbers() {
			if c := sh.season(s); c.ok {
				images = append(images, previewOf("Season "+strconv.Itoa(s), c))
			}
		}
		shown := 0
		for _, s := range sh.seasonNumbers() {
			for _, e := range sh.structure[s] {
				if c := sh.card(s, e); c.ok && shown < 24 {
					images = append(images, previewOf("S"+strconv.Itoa(s)+"E"+strconv.Itoa(e), c))
					shown++
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"tmdb": tmdb, "how": how, "images": images})
}

// relayImage fetches a Mediux image for Plex, used only when image delivery
// is set to "relay" (for networks where Plex cannot reach Mediux directly).
func (ui *webUI) relayImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !assetID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	q := url.Values{}
	if v := r.URL.Query().Get("key"); v == "jpg" {
		q.Set("key", v)
	}
	if v := r.URL.Query().Get("v"); v != "" && len(v) <= 14 {
		q.Set("v", v)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, imageHost+id+"?"+q.Encode(), nil)
	if err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; plex-plugin-mediux)")
	res, err := ui.relay.Do(req)
	if err != nil {
		http.Error(w, "Mediux unavailable", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		http.Error(w, "Mediux returned "+res.Status, http.StatusBadGateway)
		return
	}
	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		http.Error(w, "not an image", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if _, err := io.Copy(w, io.LimitReader(res.Body, 40<<20)); err != nil && !errors.Is(err, context.Canceled) {
		return
	}
}

func (ui *webUI) plexAuthStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	session, link, err := ui.auth.start(ctx)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"session": session, "url": link})
}

func (ui *webUI) plexAuthCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	done, servers, err := ui.auth.check(ctx, r.URL.Query().Get("session"))
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	if servers == nil {
		servers = []foundServer{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"done": done, "servers": servers})
}

func (ui *webUI) plexAuthAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Session  string   `json:"session"`
		Machines []string `json:"machines"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "could not read the request"})
		return
	}
	n, err := ui.auth.add(in.Session, in.Machines)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "Added " + plural(n, "server", "servers") + "."})
}
