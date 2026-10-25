package main

// Reviewing before artwork reaches Plex. In "approved" mode Plex only gets
// Mediux art for titles you've approved; these endpoints approve titles (one
// or many), mark titles to keep Plex's own art, and show what the provider
// would choose so you can compare it with what Plex shows now.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type reviewTitle struct {
	Type  mediaType `json:"type"`
	TMDB  string    `json:"tmdb"`
	Title string    `json:"title"`
	Year  int       `json:"year"`
}

const reviewBatchMax = 300

func readReviewTitles(r *http.Request, extra any) ([]reviewTitle, error) {
	var in struct {
		Items []reviewTitle `json:"items"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	if extra != nil {
		if err := json.Unmarshal(raw, extra); err != nil {
			return nil, err
		}
	}
	var out []reviewTitle
	for _, t := range in.Items {
		if (t.Type == typeMovie || t.Type == typeShow) && numericID(t.TMDB) && len(out) < reviewBatchMax {
			if len(t.Title) > 200 {
				t.Title = t.Title[:200]
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// providerChoice is the set the creator rules pick for each part of a title.
func (ui *webUI) providerChoice(ctx context.Context, t reviewTitle) (userPick, choice, error) {
	b, err := ui.provider.mediux.artwork(ctx, t.Type, t.TMDB)
	if err != nil {
		return userPick{}, choice{}, err
	}
	cfg := ui.settings.get()
	p := userPick{Type: t.Type, TMDB: t.TMDB, Title: t.Title, Year: t.Year}
	poster := choosePoster(b, cfg, "en", override{})
	if poster.ok {
		p.PosterSet = poster.art.SetID
	}
	if c := chooseWithPoster(b, cfg, kindBackdrop, "en", poster, override{}); c.ok {
		p.BackSet = c.art.SetID
	}
	if t.Type == typeShow {
		p.SeasonSet = chooseSet(b, cfg, kindSeason, "en").ID
		p.CardSet = chooseSet(b, cfg, kindStill, "en").ID
	}
	return p, poster, nil
}

// approve saves the provider's current choice as the pick for each title,
// then asks Plex to refresh them.
func (ui *webUI) approve(w http.ResponseWriter, r *http.Request) {
	titles, err := readReviewTitles(r, nil)
	if err != nil || len(titles) == 0 {
		writeJSON(w, 400, map[string]string{"error": "choose at least one title"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var saved []userPick
	var nothing []string
	for _, t := range titles {
		choice, _, err := ui.providerChoice(ctx, t)
		if err != nil {
			ui.stopApproving(w, saved, 502, err)
			return
		}
		if !choice.hasSets() {
			nothing = append(nothing, t.Title)
			continue
		}
		// Keep any part already picked by hand; fill in the rest.
		if old := ui.provider.picks.get(t.Type, t.TMDB); old != nil && !old.KeepPlex {
			for _, part := range []struct {
				dst *string
				src string
			}{
				{&choice.PosterSet, old.PosterSet}, {&choice.BackSet, old.BackSet},
				{&choice.SeasonSet, old.SeasonSet}, {&choice.CardSet, old.CardSet}} {
				if part.src != "" {
					*part.dst = part.src
				}
			}
		}
		if err := ui.provider.picks.put(choice); err != nil {
			ui.stopApproving(w, saved, 500, err)
			return
		}
		saved = append(saved, choice)
	}
	ui.refreshLater(saved, "Approved")
	status := "Approved " + plural(len(saved), "title", "titles") + "."
	if len(nothing) > 0 {
		status += " Mediux has nothing your settings allow for " + strings.Join(nothing, ", ") + "."
	}
	if len(saved) > 0 && ui.canRefresh() {
		status += " Refreshing them in Plex now; the Log shows each one."
	}
	writeJSON(w, 200, map[string]any{"status": status, "approved": len(saved)})
}

// stopApproving ends a batch early: the titles already approved still get
// refreshed and logged, and the message says how far it got.
func (ui *webUI) stopApproving(w http.ResponseWriter, saved []userPick, status int, err error) {
	ui.refreshLater(saved, "Approved")
	msg := err.Error()
	if len(saved) > 0 {
		msg = "Approved " + plural(len(saved), "title", "titles") + ", then stopped: " + msg
	}
	writeJSON(w, status, map[string]any{"error": msg, "approved": len(saved)})
}

// keepPlex marks titles to keep Plex's own artwork (or undoes that).
func (ui *webUI) keepPlex(w http.ResponseWriter, r *http.Request) {
	var flag struct {
		Keep bool `json:"keep"`
	}
	titles, err := readReviewTitles(r, &flag)
	if err != nil || len(titles) == 0 {
		writeJSON(w, 400, map[string]string{"error": "choose at least one title"})
		return
	}
	var changed []userPick
	for _, t := range titles {
		p := userPick{Type: t.Type, TMDB: t.TMDB, Title: t.Title, Year: t.Year}
		if old := ui.provider.picks.get(t.Type, t.TMDB); old != nil {
			p = *old
		}
		p.KeepPlex = flag.Keep
		if err := ui.provider.picks.put(p); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		changed = append(changed, p)
	}
	verb := "Keeping Plex's artwork for "
	if !flag.Keep {
		verb = "Back to review for "
	}
	ui.refreshLater(changed, strings.TrimSuffix(verb, " for "))
	writeJSON(w, 200, map[string]string{"status": verb + plural(len(changed), "title", "titles") + "."})
}

func (ui *webUI) canRefresh() bool {
	cfg := ui.settings.get()
	return cfg.RefreshPlex && hasPlexToken(cfg.PlexServers)
}

// refreshLater asks Plex to refresh titles one by one in the background.
func (ui *webUI) refreshLater(picks []userPick, what string) {
	if len(picks) == 0 || !ui.canRefresh() {
		for _, p := range picks {
			ui.activity.add(string(p.Type), p.Title, "picked", what+"; refresh the title in Plex to see it")
		}
		return
	}
	cfg := ui.settings.get()
	go func() {
		for _, p := range picks {
			bg, done := context.WithTimeout(context.Background(), 3*time.Minute)
			res, err := ui.plex.refresh(bg, cfg.PlexServers, p.Type, p.TMDB, cfg.ReplaceLocked)
			done()
			detail, result := what+"; "+res.describe(), "picked"
			if err != nil {
				detail, result = detail+"; Plex refresh problem: "+err.Error(), "error"
				log.Printf("review refresh: %v", err)
			}
			ui.activity.add(string(p.Type), p.Title, result, detail)
		}
	}()
}

// choices answers GET /api/review/choices?keys=movie:603,show:1413 with the
// poster the provider would pick for each title.
func (ui *webUI) choices(w http.ResponseWriter, r *http.Request) {
	keys := strings.Split(r.URL.Query().Get("keys"), ",")
	if len(keys) > 250 {
		keys = keys[:250]
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	type result struct {
		Poster  string `json:"poster,omitempty"`
		Creator string `json:"creator,omitempty"`
	}
	out := map[string]result{}
	for _, k := range keys {
		typ, id, _ := strings.Cut(k, ":")
		t := reviewTitle{Type: mediaType(typ), TMDB: id}
		if t.Type != typeMovie && t.Type != typeShow || !numericID(id) {
			continue
		}
		_, poster, err := ui.providerChoice(ctx, t)
		if err != nil {
			break // Mediux trouble: send what we have
		}
		if poster.ok {
			out[k] = result{imageURL(poster.art, "optimized"), poster.art.Creator}
		} else {
			out[k] = result{}
		}
	}
	writeJSON(w, 200, map[string]any{"choices": out})
}
