package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Consistency modes for season posters and title cards.
const (
	modeMostComplete    = "MostComplete"    // complete set first, then creator priority
	modeCreatorPriority = "CreatorPriority" // creator priority first, then most artwork
	modeSeriesPoster    = "SeriesPoster"    // same set as the show poster
	modeIndependent     = "Independent"     // pick each image on its own
)

type settings struct {
	Token                string   `json:"api_token"`
	PriorityCreators     []string `json:"priority_creators"`
	ExcludedCreators     []string `json:"excluded_creators"`
	OnlyPriority         bool     `json:"only_priority_creators"`
	SeasonConsistency    string   `json:"season_consistency"`
	TitlecardConsistency string   `json:"titlecard_consistency"`
	MissingArtwork       string   `json:"missing_artwork"` // Fallback or KeepExisting
	Quality              string   `json:"quality"`         // optimized or original
	ImageDelivery        string   `json:"image_delivery"`  // direct or relay
	CacheHours           int      `json:"cache_hours"`
}

func defaultSettings() settings {
	return settings{
		PriorityCreators:     []string{},
		ExcludedCreators:     []string{},
		SeasonConsistency:    modeMostComplete,
		TitlecardConsistency: modeMostComplete,
		MissingArtwork:       "Fallback",
		Quality:              "optimized",
		ImageDelivery:        "direct",
		CacheHours:           24,
	}
}

func (s settings) validate() error {
	modes := map[string]bool{modeMostComplete: true, modeCreatorPriority: true, modeSeriesPoster: true, modeIndependent: true}
	if !modes[s.SeasonConsistency] || !modes[s.TitlecardConsistency] {
		return errors.New("unknown consistency setting")
	}
	if s.MissingArtwork != "Fallback" && s.MissingArtwork != "KeepExisting" {
		return errors.New("missing artwork must be Fallback or KeepExisting")
	}
	if s.Quality != "optimized" && s.Quality != "original" {
		return errors.New("image quality must be optimized or original")
	}
	if s.ImageDelivery != "direct" && s.ImageDelivery != "relay" {
		return errors.New("image delivery must be direct or relay")
	}
	if s.CacheHours < 0 || s.CacheHours > 168 {
		return errors.New("cache hours must be between 0 and 168")
	}
	for _, list := range [][]string{s.PriorityCreators, s.ExcludedCreators} {
		if len(list) > 200 {
			return errors.New("too many creators")
		}
		for _, name := range list {
			if name == "" || len(name) > 128 || strings.ContainsAny(name, ",\r\n") {
				return fmt.Errorf("invalid creator name %q", name)
			}
		}
	}
	return nil
}

// cleanCreators trims, drops blanks and removes case-insensitive duplicates.
func cleanCreators(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}
	return out
}

// settingsStore keeps settings in memory and in <config dir>/settings.json.
type settingsStore struct {
	path      string
	mu        sync.RWMutex
	current   settings
	listeners []func()
}

func loadSettings(dir string) (*settingsStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st := &settingsStore{path: filepath.Join(dir, "settings.json"), current: defaultSettings()}
	raw, err := os.ReadFile(st.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	loaded := defaultSettings()
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("reading %s: %w", st.path, err)
	}
	loaded.PriorityCreators = cleanCreators(loaded.PriorityCreators)
	loaded.ExcludedCreators = cleanCreators(loaded.ExcludedCreators)
	if err := loaded.validate(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", st.path, err)
	}
	st.current = loaded
	return st, nil
}

func (st *settingsStore) get() settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	s := st.current
	s.PriorityCreators = append([]string{}, s.PriorityCreators...)
	s.ExcludedCreators = append([]string{}, s.ExcludedCreators...)
	return s
}

func (st *settingsStore) onChange(fn func()) {
	st.mu.Lock()
	st.listeners = append(st.listeners, fn)
	st.mu.Unlock()
}

// save validates, writes atomically, then applies the new settings.
func (st *settingsStore) save(s settings) error {
	s.PriorityCreators = cleanCreators(s.PriorityCreators)
	s.ExcludedCreators = cleanCreators(s.ExcludedCreators)
	if err := s.validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return err
	}
	st.mu.Lock()
	st.current = s
	listeners := append([]func(){}, st.listeners...)
	st.mu.Unlock()
	for _, fn := range listeners {
		fn()
	}
	return nil
}
