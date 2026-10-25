package main

// Picks: which artwork wins for a title, in this order:
//  1. a set you picked on the settings page (Sets tab)
//  2. a set you saved in AURA (if AURA.db is connected)
//  3. creator priority and the consistency rules
//
// Settings-page picks remember the set, not single images, so artwork the
// creator adds to that set later (a new title card, say) is used too.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// userPick is a set chosen on the settings page, per part of the artwork.
type userPick struct {
	Type      mediaType `json:"type"`
	TMDB      string    `json:"tmdb"`
	Title     string    `json:"title"`
	Year      int       `json:"year,omitempty"`
	PosterSet string    `json:"poster_set,omitempty"`
	BackSet   string    `json:"background_set,omitempty"`
	SeasonSet string    `json:"season_set,omitempty"`
	CardSet   string    `json:"titlecard_set,omitempty"`
	KeepPlex  bool      `json:"keep_plex,omitempty"` // never give this title Mediux art
	Updated   time.Time `json:"updated"`
}

func (u userPick) empty() bool {
	return !u.KeepPlex && !u.hasSets()
}

func (u userPick) hasSets() bool {
	return u.PosterSet != "" || u.BackSet != "" || u.SeasonSet != "" || u.CardSet != ""
}

// parts describes what a pick covers, for the settings page.
func (u userPick) parts() string {
	if u.KeepPlex {
		return "keeps Plex art"
	}
	var out []string
	for _, p := range []struct{ set, name string }{{u.PosterSet, "poster"}, {u.BackSet, "background"},
		{u.SeasonSet, "season posters"}, {u.CardSet, "title cards"}} {
		if p.set != "" {
			out = append(out, p.name)
		}
	}
	return strings.Join(out, ", ")
}

type pickStore struct {
	path  string
	mu    sync.Mutex
	picks map[string]userPick // "movie:603"
}

func loadPicks(dir string) *pickStore {
	ps := &pickStore{path: filepath.Join(dir, "picks.json"), picks: map[string]userPick{}}
	if raw, err := os.ReadFile(ps.path); err == nil {
		_ = json.Unmarshal(raw, &ps.picks)
	}
	return ps
}

func (ps *pickStore) get(typ mediaType, tmdb string) *userPick {
	if ps == nil {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p, ok := ps.picks[string(typ)+":"+tmdb]; ok {
		return &p
	}
	return nil
}

func (ps *pickStore) list() []userPick {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]userPick, 0, len(ps.picks))
	for _, p := range ps.picks {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// put saves a pick; an empty pick removes it.
func (ps *pickStore) put(p userPick) error {
	if p.Type != typeMovie && p.Type != typeShow {
		return errors.New("unknown title type")
	}
	if !numericID(p.TMDB) {
		return errors.New("unknown title")
	}
	if p.Type == typeMovie {
		p.SeasonSet, p.CardSet = "", ""
	}
	if p.KeepPlex { // keeping Plex art replaces any sets picked before
		p.PosterSet, p.BackSet, p.SeasonSet, p.CardSet = "", "", "", ""
	}
	p.Updated = time.Now()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	key := string(p.Type) + ":" + p.TMDB
	if p.empty() {
		delete(ps.picks, key)
	} else {
		ps.picks[key] = p
	}
	raw, err := json.MarshalIndent(ps.picks, "", "  ")
	if err != nil {
		return err
	}
	tmp := ps.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ps.path)
}

func numericID(s string) bool {
	if s == "" || len(s) > 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// override bundles everything that can overrule creator priority for a title.
type override struct {
	pick *userPick
	aura *auraPin
}

// choice is one picked image and where it came from.
type choice struct {
	art    artwork
	ok     bool
	source string // "", "pick" (settings page) or "aura"
}

func (o override) set(kind string) string {
	if o.pick == nil {
		return ""
	}
	switch kind {
	case kindPoster, kindLogo:
		return o.pick.PosterSet
	case kindBackdrop:
		return o.pick.BackSet
	case kindSeason:
		return o.pick.SeasonSet
	case kindStill:
		return o.pick.CardSet
	}
	return ""
}

// fromSet finds the best image of one kind inside one set. Creator exclusions
// don't apply: you chose this set yourself.
func fromSet(b bundle, setID, kind string, season, episode int, language string) (artwork, bool) {
	if setID == "" {
		return artwork{}, false
	}
	var list []artwork
	for _, s := range b.Sets {
		if s.ID != setID {
			continue
		}
		for _, a := range s.Art {
			if a.Kind == kind && a.Season == season && a.Episode == episode {
				list = append(list, a)
			}
		}
	}
	return bestForLanguage(list, kind, language)
}

func (o override) auraID(kind string, season, episode int) string {
	if o.aura == nil {
		return ""
	}
	switch kind {
	case kindPoster:
		return o.aura.Poster
	case kindBackdrop:
		return o.aura.Backdrop
	case kindSeason:
		return o.aura.Seasons[season]
	case kindStill:
		return o.aura.Cards[[2]int{season, episode}]
	}
	return ""
}

// pinnedChoice returns the settings-page pick, else the AURA pick, if any.
func (o override) pinnedChoice(b bundle, kind string, season, episode int, language string) (choice, bool) {
	if a, ok := fromSet(b, o.set(kind), kind, season, episode, language); ok {
		return choice{a, true, "pick"}, true
	}
	if a, ok := pinned(b, o.auraID(kind, season, episode)); ok {
		return choice{a, true, "aura"}, true
	}
	return choice{}, false
}

// choosePoster: a pick if there is one, else creator priority.
func choosePoster(b bundle, cfg settings, language string, o override) choice {
	if c, ok := o.pinnedChoice(b, kindPoster, 0, 0, language); ok {
		return c
	}
	a, ok := best(b, cfg, kindPoster, language)
	return choice{a, ok, ""}
}

// chooseWithPoster picks a background or logo, preferring the poster's set.
func chooseWithPoster(b bundle, cfg settings, kind, language string, poster choice, o override) choice {
	if c, ok := o.pinnedChoice(b, kind, 0, 0, language); ok {
		return c
	}
	set := ""
	if poster.ok {
		set = poster.art.SetID
	}
	a, ok := pickInSet(b, cfg, kind, 0, 0, language, setChoice{ID: set})
	return choice{a, ok, ""}
}

// chooseEach picks a season poster or title card: a pick, else the rules.
func chooseEach(b bundle, cfg settings, kind string, season, episode int, language string, rule setChoice, o override) choice {
	if c, ok := o.pinnedChoice(b, kind, season, episode, language); ok {
		return c
	}
	a, ok := pickInSet(b, cfg, kind, season, episode, language, rule)
	return choice{a, ok, ""}
}
