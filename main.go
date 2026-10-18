// plex-plugin-mediux serves Mediux artwork to Plex as a custom metadata
// provider, with a settings page for the Mediux token and artwork rules.
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
	"time"
)

// version is set at build time.
var version = "dev"

func main() {
	listen := flag.String("listen", ":32600", "address to listen on")
	configDir := flag.String("config", "/config", "folder for saved settings")
	flag.Parse()

	store, err := loadSettings(*configDir)
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	mediux := newMediuxClient(store)
	ids := newIdentifier(mediux)
	activity := &activityLog{}
	plex := &plexProvider{mediux: mediux, ids: ids, settings: store, activity: activity}
	ui := &webUI{settings: store, mediux: mediux, ids: ids, activity: activity,
		relay: &http.Client{Timeout: 60 * time.Second}}

	mux := http.NewServeMux()
	ui.routes(mux)
	for _, p := range []string{"/movies", "/movies/", "/tv", "/tv/"} {
		mux.Handle(p, plex)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("plex-plugin-mediux %s listening on %s (settings saved in %s)", version, *listen, *configDir)
	if store.get().Token == "" {
		log.Printf("no Mediux API token yet: open the settings page in a browser to add one")
	}
	log.Fatal(srv.ListenAndServe())
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logRequests logs Plex's provider calls (not the settings page or images).
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/movies") || strings.HasPrefix(r.URL.Path, "/tv") {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}
