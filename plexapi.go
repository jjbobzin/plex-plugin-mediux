package main

// Talking to Plex servers directly, used only to refresh an item when the
// watcher finds new artwork. Items are found by their TMDB ID, which Plex
// Movie / Plex Series record on every matched item.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type plexIndex struct {
	built time.Time
	items map[string][]string // "movie:603" -> Plex ratingKeys
}

type plexClient struct {
	http *http.Client

	mu      sync.Mutex
	indexes map[string]*plexIndex // per server URL
}

func newPlexClient() *plexClient {
	return &plexClient{http: &http.Client{Timeout: 60 * time.Second}, indexes: map[string]*plexIndex{}}
}

func (pc *plexClient) do(ctx context.Context, srv plexServer, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(srv.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", srv.Token)
	req.Header.Set("X-Plex-Client-Identifier", "plex-plugin-mediux")
	req.Header.Set("X-Plex-Product", "plex-plugin-mediux")
	res, err := pc.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", srv.Name, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s rejected the Plex token", srv.Name)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("%s answered %s", srv.Name, res.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 256<<20)).Decode(out)
}

// identify checks a server's address and token, returning its friendly name.
func (pc *plexClient) identify(ctx context.Context, srv plexServer) (string, error) {
	var out struct {
		MediaContainer struct {
			FriendlyName string `json:"friendlyName"`
		} `json:"MediaContainer"`
	}
	if err := pc.do(ctx, srv, http.MethodGet, "/", &out); err != nil {
		return "", err
	}
	return out.MediaContainer.FriendlyName, nil
}

// index maps TMDB IDs to ratingKeys for one server's movie and show libraries.
func (pc *plexClient) index(ctx context.Context, srv plexServer, maxAge time.Duration) (*plexIndex, error) {
	pc.mu.Lock()
	idx := pc.indexes[srv.URL]
	pc.mu.Unlock()
	if idx != nil && time.Since(idx.built) < maxAge {
		return idx, nil
	}
	var sections struct {
		MediaContainer struct {
			Directory []struct {
				Key  string `json:"key"`
				Type string `json:"type"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := pc.do(ctx, srv, http.MethodGet, "/library/sections", &sections); err != nil {
		return nil, err
	}
	built := &plexIndex{built: time.Now(), items: map[string][]string{}}
	for _, d := range sections.MediaContainer.Directory {
		kind := map[string]mediaType{"movie": typeMovie, "show": typeShow}[d.Type]
		if kind == "" {
			continue
		}
		var all struct {
			MediaContainer struct {
				Metadata []struct {
					RatingKey string `json:"ratingKey"`
					Guid      []struct {
						ID string `json:"id"`
					} `json:"Guid"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		path := "/library/sections/" + url.PathEscape(d.Key) + "/all?includeGuids=1"
		if err := pc.do(ctx, srv, http.MethodGet, path, &all); err != nil {
			return nil, err
		}
		for _, m := range all.MediaContainer.Metadata {
			for _, g := range m.Guid {
				if id, ok := strings.CutPrefix(g.ID, "tmdb://"); ok {
					k := string(kind) + ":" + id
					built.items[k] = append(built.items[k], m.RatingKey)
				}
			}
		}
	}
	pc.mu.Lock()
	pc.indexes[srv.URL] = built
	pc.mu.Unlock()
	return built, nil
}

// refresh asks every configured server holding this title to refresh it.
// It returns the names of the servers that were asked.
func (pc *plexClient) refresh(ctx context.Context, servers []plexServer, typ mediaType, tmdb string) ([]string, error) {
	var asked []string
	var errs []error
	for _, srv := range servers {
		if srv.Token == "" {
			continue
		}
		idx, err := pc.index(ctx, srv, 6*time.Hour)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		keys := idx.items[string(typ)+":"+tmdb]
		if len(keys) == 0 && time.Since(idx.built) > 15*time.Minute {
			// Possibly added since the index was built: rebuild once and retry.
			if idx, err = pc.index(ctx, srv, 0); err == nil {
				keys = idx.items[string(typ)+":"+tmdb]
			}
		}
		for _, k := range keys {
			if err := pc.do(ctx, srv, http.MethodPut, "/library/metadata/"+url.PathEscape(k)+"/refresh", nil); err != nil {
				errs = append(errs, err)
				continue
			}
			asked = append(asked, srv.Name)
		}
	}
	return asked, errors.Join(errs...)
}
