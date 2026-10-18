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
	settings *settingsStore
	mediux   *mediuxClient
	ids      *identifier
	activity *activityLog
	relay    *http.Client
}

func (ui *webUI) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", ui.page)
	mux.HandleFunc("GET /api/settings", ui.getSettings)
	mux.HandleFunc("POST /api/settings", ui.guard(ui.saveSettings))
	mux.HandleFunc("POST /api/check", ui.guard(ui.check))
	mux.HandleFunc("GET /api/creators", ui.creators)
	mux.HandleFunc("GET /api/activity", ui.recent)
	mux.HandleFunc("GET /api/preview", ui.preview)
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

type publicSettings struct {
	settings
	Token           string `json:"api_token,omitempty"`
	TokenConfigured bool   `json:"token_configured"`
	Version         string `json:"version"`
}

func (ui *webUI) getSettings(w http.ResponseWriter, _ *http.Request) {
	s := ui.settings.get()
	configured := strings.TrimSpace(s.Token) != ""
	s.Token = ""
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, publicSettings{settings: s, TokenConfigured: configured, Version: version})
}

func (ui *webUI) saveSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		settings
		ClearToken bool `json:"clear_token"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "could not read settings"})
		return
	}
	next := in.settings
	current := ui.settings.get()
	next.Token = strings.TrimSpace(next.Token)
	if next.Token == "" && !in.ClearToken {
		next.Token = current.Token // blank means keep the saved token
	}
	if err := ui.settings.save(next); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
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
	if err := ui.mediux.checkToken(ctx, token); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "Connected to Mediux."})
}

func (ui *webUI) creators(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if n := len([]rune(q)); n < 2 || n > 80 {
		writeJSON(w, 400, map[string]string{"error": "Type 2 to 80 characters."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	names, err := ui.mediux.creators(ctx, q)
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

type previewImage struct {
	Label   string `json:"label"`
	URL     string `json:"url"`
	Creator string `json:"creator"`
}

// preview shows what would be chosen for a movie or show, without Plex.
func (ui *webUI) preview(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kind := r.URL.Query().Get("type")
	if q == "" || (kind != "movie" && kind != "show") {
		writeJSON(w, 400, map[string]string{"error": "Enter a title or TMDB ID."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// Titles come first: shows like "1923" or "24" are numbers. A number with
	// no exact title match is treated as a TMDB ID.
	var tmdb, how string
	var err error
	title, year := splitYear(q, 0)
	h := hints{Title: title, Year: year}
	if kind == "movie" {
		tmdb, how, err = ui.ids.movie(ctx, h)
	} else {
		tmdb, how, err = ui.ids.show(ctx, q, h)
	}
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	if tmdb == "" {
		if _, numErr := strconv.Atoi(q); numErr == nil {
			tmdb, how = q, "TMDB ID"
		} else {
			writeJSON(w, 404, map[string]string{"error": "No exact title match. Try the title with its year, like \"1923 (2022)\", or the TMDB ID."})
			return
		}
	}
	cfg := ui.settings.get()
	direct := func(a artwork) string { return imageURL(a, cfg.Quality) }
	images := []previewImage{}
	if kind == "movie" {
		b, err := ui.mediux.artwork(ctx, typeMovie, tmdb)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		for _, k := range []struct{ kind, label string }{{kindPoster, "Poster"}, {kindBackdrop, "Background"}, {kindLogo, "Logo"}} {
			if a, ok := best(b, cfg, k.kind, "en"); ok {
				images = append(images, previewImage{k.label, direct(a), a.Creator})
			}
		}
	} else {
		pr := &plexRequest{language: "en"}
		sh, err := (&plexProvider{mediux: ui.mediux, settings: ui.settings}).loadShow(ctx, pr, tmdb)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		for _, k := range []struct{ kind, label string }{{kindPoster, "Poster"}, {kindBackdrop, "Background"}, {kindLogo, "Logo"}} {
			if a, ok := best(sh.b, cfg, k.kind, "en"); ok {
				images = append(images, previewImage{k.label, direct(a), a.Creator})
			}
		}
		for _, s := range sh.seasonNumbers() {
			if a, ok := pickInSet(sh.b, cfg, kindSeason, s, 0, "en", sh.seasons); ok {
				images = append(images, previewImage{"Season " + strconv.Itoa(s), direct(a), a.Creator})
			}
		}
		shown := 0
		for _, s := range sh.seasonNumbers() {
			for _, e := range sh.structure[s] {
				if a, ok := pickInSet(sh.b, cfg, kindStill, s, e, "en", sh.cards); ok && shown < 24 {
					images = append(images, previewImage{"S" + strconv.Itoa(s) + "E" + strconv.Itoa(e), direct(a), a.Creator})
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
