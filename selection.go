package main

// Artwork selection rules: creator priority, exclusions, language preference
// and season-poster / title-card set consistency. Adapted from Pukabyte's
// silo-plugin-mediux (https://github.com/Pukabyte/silo-plugin-mediux,
// AGPL-3.0-or-later).

import (
	"sort"
	"strings"
)

const unranked = 99999999

func rankMap(names []string) map[string]int {
	out := map[string]int{}
	for i, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			if _, dup := out[n]; !dup {
				out[n] = i
			}
		}
	}
	return out
}

// eligible reports a set's creator rank and whether the set may be used at all.
func eligible(s set, cfg settings, priority, excluded map[string]int) (int, bool) {
	creator := strings.ToLower(s.Creator)
	rank, preferred := priority[creator]
	if _, blocked := excluded[creator]; blocked || cfg.OnlyPriority && !preferred {
		return 0, false
	}
	if !preferred {
		rank = unranked
	}
	return rank, true
}

// candidates lists every eligible image of one kind for one season/episode,
// best creator first, then most popular set.
func candidates(b bundle, cfg settings, kind string, season, episode int) []artwork {
	priority, excluded := rankMap(cfg.PriorityCreators), rankMap(cfg.ExcludedCreators)
	var out []artwork
	for _, s := range b.Sets {
		rank, ok := eligible(s, cfg, priority, excluded)
		if !ok {
			continue
		}
		for _, a := range s.Art {
			if a.Kind != kind || a.Season != season || a.Episode != episode {
				continue
			}
			a.Popularity = s.Popularity
			a.Rank = rank
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		if out[i].Popularity != out[j].Popularity {
			return out[i].Popularity > out[j].Popularity
		}
		return out[i].ID < out[j].ID
	})
	seen := map[string]bool{}
	dedup := out[:0]
	for _, a := range out {
		if !seen[a.ID] {
			seen[a.ID] = true
			dedup = append(dedup, a)
		}
	}
	return dedup
}

// bestForLanguage keeps creator ranking inside each language tier.
func bestForLanguage(list []artwork, kind, requested string) (artwork, bool) {
	if len(list) == 0 {
		return artwork{}, false
	}
	// Title cards keep creator priority ahead of language.
	if kind == kindStill {
		end := 1
		for end < len(list) && list[end].Rank == list[0].Rank {
			end++
		}
		list = list[:end]
	}
	var tiers []func(string) bool
	switch kind {
	case kindPoster, kindSeason:
		if requested != "" {
			tiers = append(tiers, func(l string) bool { return l == requested })
		}
		if requested != "en" {
			tiers = append(tiers, func(l string) bool { return l == "en" })
		}
		tiers = append(tiers, func(l string) bool { return l != "" }, func(l string) bool { return l == "" })
	case kindLogo, kindStill:
		if requested != "" {
			tiers = append(tiers, func(l string) bool { return l == requested })
		}
		if requested != "en" {
			tiers = append(tiers, func(l string) bool { return l == "en" })
		}
		tiers = append(tiers, func(l string) bool { return l == "" }, func(l string) bool { return l != "" })
	default: // backgrounds favor artwork without text
		tiers = append(tiers, func(l string) bool { return l == "" }, func(l string) bool { return l != "" })
	}
	for _, match := range tiers {
		for _, a := range list {
			if match(a.Language) {
				return a, true
			}
		}
	}
	return artwork{}, false
}

// setChoice is the set that season posters or title cards should come from.
type setChoice struct {
	ID     string
	Strict bool // true: never fall back to another set
}

// chooseSet is computed once per show for season posters or title cards.
func chooseSet(b bundle, cfg settings, kind, language string) setChoice {
	mode := cfg.SeasonConsistency
	if kind == kindStill {
		mode = cfg.TitlecardConsistency
	}
	choice := setChoice{Strict: mode != modeIndependent && cfg.MissingArtwork == "KeepExisting"}
	switch mode {
	case modeMostComplete, modeCreatorPriority:
		choice.ID = consistentSet(b, cfg, kind, language, mode == modeCreatorPriority)
	case modeSeriesPoster:
		if a, ok := bestForLanguage(candidates(b, cfg, kindPoster, 0, 0), kindPoster, language); ok {
			choice.ID = a.SetID
		}
	}
	return choice
}

// pickInSet picks one image, preferring the chosen set.
func pickInSet(b bundle, cfg settings, kind string, season, episode int, language string, choice setChoice) (artwork, bool) {
	all := candidates(b, cfg, kind, season, episode)
	if len(all) == 0 {
		return artwork{}, false
	}
	if choice.ID != "" {
		var local []artwork
		for _, a := range all {
			if a.SetID == choice.ID {
				local = append(local, a)
			}
		}
		if a, ok := bestForLanguage(local, kind, language); ok {
			return a, true
		}
	}
	if choice.Strict {
		return artwork{}, false
	}
	return bestForLanguage(all, kind, language)
}

// best picks the automatic image for posters, backgrounds and logos. Backgrounds
// and logos prefer the set the poster came from.
func best(b bundle, cfg settings, kind, language string) (artwork, bool) {
	if kind == kindPoster {
		return bestForLanguage(candidates(b, cfg, kindPoster, 0, 0), kindPoster, language)
	}
	poster, _ := bestForLanguage(candidates(b, cfg, kindPoster, 0, 0), kindPoster, language)
	return pickInSet(b, cfg, kind, 0, 0, language, setChoice{ID: poster.SetID})
}

// consistentSet ranks sets for one category. Completeness counts the distinct
// seasons or episodes that Mediux has artwork for, not the local library.
func consistentSet(b bundle, cfg settings, kind, language string, creatorFirst bool) string {
	priority, excluded := rankMap(cfg.PriorityCreators), rankMap(cfg.ExcludedCreators)
	expected := map[[2]int]bool{}
	for _, s := range b.Sets {
		for scope := range scopes(s, kind) {
			expected[scope] = true
		}
	}
	type score struct {
		id                       string
		rank, coverage, language int
		popularity               float64
		complete                 bool
	}
	better := func(c, best score) bool {
		if !creatorFirst && c.complete != best.complete {
			return c.complete
		}
		if c.rank != best.rank {
			return c.rank < best.rank
		}
		if c.coverage != best.coverage {
			return c.coverage > best.coverage
		}
		if c.language != best.language {
			return c.language < best.language
		}
		if c.popularity != best.popularity {
			return c.popularity > best.popularity
		}
		return c.id < best.id
	}
	var winner *score
	for _, s := range b.Sets {
		rank, ok := eligible(s, cfg, priority, excluded)
		if !ok || s.ID == "" {
			continue
		}
		sc := scopes(s, kind)
		if len(sc) == 0 {
			continue
		}
		tier := 99
		for _, a := range s.Art {
			if a.Kind == kind {
				if t := languageTier(a.Language, language, kind); t < tier {
					tier = t
				}
			}
		}
		c := score{s.ID, rank, len(sc), tier, s.Popularity, len(sc) == len(expected)}
		if winner == nil || better(c, *winner) {
			winner = &c
		}
	}
	if winner == nil {
		return ""
	}
	return winner.id
}

func scopes(s set, kind string) map[[2]int]bool {
	out := map[[2]int]bool{}
	for _, a := range s.Art {
		if a.Kind != kind || !assetID.MatchString(a.ID) || a.Season < 0 || kind == kindStill && a.Episode <= 0 {
			continue
		}
		episode := 0
		if kind == kindStill {
			episode = a.Episode
		}
		out[[2]int{a.Season, episode}] = true
	}
	return out
}

func languageTier(language, requested, kind string) int {
	if kind == kindStill {
		if language == "" {
			return 0
		}
		return 1
	}
	switch {
	case requested != "" && language == requested:
		return 0
	case language == "en":
		return 1
	case language != "":
		return 2
	}
	return 3
}
