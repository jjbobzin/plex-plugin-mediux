package main

import (
	"net/http"
	"time"
)

// app is the whole provider: Plex endpoints, settings page and background work.
type app struct {
	store    *settingsStore
	mediux   *mediuxClient
	ids      *identifier
	activity *activityLog
	provider *plexProvider
	plexAPI  *plexClient
	discord  *discord
	watch    *watcher
	mux      *http.ServeMux
}

func newApp(store *settingsStore, configDir string) *app {
	a := &app{store: store, activity: &activityLog{}, plexAPI: newPlexClient(), discord: newDiscord(store)}
	a.mediux = newMediuxClient(store)
	a.ids = newIdentifier(a.mediux)
	aura := newAuraStore(func() string { return store.get().AuraDB })
	a.provider = &plexProvider{mediux: a.mediux, ids: a.ids, settings: store, activity: a.activity, aura: aura}
	a.watch = newWatcher(a.provider, a.plexAPI, a.discord, configDir)
	a.provider.watch = a.watch
	ui := &webUI{settings: store, provider: a.provider, plex: a.plexAPI, discord: a.discord, activity: a.activity,
		relay: &http.Client{Timeout: 60 * time.Second}, started: time.Now()}
	a.mux = http.NewServeMux()
	ui.routes(a.mux)
	for _, p := range []string{"/movies", "/movies/", "/tv", "/tv/"} {
		a.mux.Handle(p, a.provider)
	}
	return a
}
