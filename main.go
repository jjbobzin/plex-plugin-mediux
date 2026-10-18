// plex-plugin-mediux serves Mediux artwork to Plex as a custom metadata
// provider, with a settings page for the Mediux token and artwork rules.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
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
	a := newApp(store, *configDir)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.watch.run(ctx)
	go a.discord.run(ctx)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           logRequests(a.mux),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("plex-plugin-mediux %s listening on %s (settings saved in %s)", version, *listen, *configDir)
	if store.get().Token == "" {
		log.Printf("no Mediux API token yet: open the settings page in a browser to add one")
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-drained // let Plex's open requests finish before the final save
	a.watch.save()
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
