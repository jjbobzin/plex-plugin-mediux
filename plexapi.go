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
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type plexRef struct {
	Key     string // ratingKey
	Section string // library section ID
	Library string // library name
	Custom  bool   // library's agent includes Mediux Artwork
	// For the Library view.
	Title   string
	Year    int
	Thumb   string // Plex image path of the current poster
	AddedAt int64  // seconds since 1970
	Type    mediaType
	TMDB    string
}

// builtInAgent reports Plex's own agents. Libraries on them never ask this
// provider for artwork, so they are never refreshed or unlocked from here.
func builtInAgent(agent string) bool {
	switch agent {
	case "", "tv.plex.agents.movie", "tv.plex.agents.series", "tv.plex.agents.none",
		"tv.plex.agents.music", "tv.plex.agents.photo":
		return true
	}
	return strings.HasPrefix(agent, "com.plexapp.agents.")
}

type plexIndex struct {
	built time.Time
	items map[string][]plexRef // "movie:603" -> where it is on this server
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
	sections, err := pc.sections(ctx, srv)
	if err != nil {
		return nil, err
	}
	built := &plexIndex{built: time.Now(), items: map[string][]plexRef{}}
	for _, d := range sections {
		if d.Type == "movie" || d.Type == "show" {
			log.Printf("Plex %s: library %q uses agent %s, agent group %q: %s", srv.Name, d.Title, d.Agent, d.Group,
				map[bool]string{true: "includes Mediux Artwork", false: "no Mediux Artwork"}[d.Mediux])
		}
		kind := map[string]mediaType{"movie": typeMovie, "show": typeShow}[d.Type]
		if kind == "" {
			continue
		}
		// Items are read as raw fields because Plex sends both "guid" (one
		// plex:// string) and "Guid" (the list of external IDs), and Go's JSON
		// decoder matches field names without regard to case.
		var all struct {
			MediaContainer struct {
				Metadata []map[string]json.RawMessage `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		path := "/library/sections/" + url.PathEscape(d.Key) + "/all?includeGuids=1"
		if err := pc.do(ctx, srv, http.MethodGet, path, &all); err != nil {
			return nil, err
		}
		for _, m := range all.MediaContainer.Metadata {
			var ratingKey string
			var guids []struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(m["ratingKey"], &ratingKey) != nil || json.Unmarshal(m["Guid"], &guids) != nil {
				continue
			}
			ref := plexRef{Key: ratingKey, Section: d.Key, Library: d.Title, Custom: d.Mediux, Type: kind}
			_ = json.Unmarshal(m["title"], &ref.Title)
			_ = json.Unmarshal(m["year"], &ref.Year)
			_ = json.Unmarshal(m["thumb"], &ref.Thumb)
			_ = json.Unmarshal(m["addedAt"], &ref.AddedAt)
			for _, g := range guids {
				if id, ok := strings.CutPrefix(g.ID, "tmdb://"); ok {
					ref.TMDB = id
					k := string(kind) + ":" + id
					built.items[k] = append(built.items[k], ref)
				}
			}
		}
	}
	pc.mu.Lock()
	pc.indexes[srv.URL] = built
	pc.mu.Unlock()
	return built, nil
}

// refreshResult says where a refresh went.
type refreshResult struct {
	Asked   []string // servers asked to refresh
	Skipped []string // "Movies on Unlimited-Primary": libraries not on a custom agent
}

// describe turns a refresh result into words for the Log.
func (r refreshResult) describe() string {
	var parts []string
	if len(r.Asked) > 0 {
		parts = append(parts, "refreshed in Plex: "+strings.Join(unique(r.Asked), ", "))
	}
	if len(r.Skipped) > 0 {
		parts = append(parts, "skipped "+strings.Join(unique(r.Skipped), ", ")+" (not on a Mediux agent)")
	}
	if len(parts) == 0 {
		return "not found on your Plex servers"
	}
	return strings.Join(parts, "; ")
}

// refresh asks every configured server holding this title to refresh it, in
// libraries that use a custom agent only: libraries on Plex's own agents
// never use this provider, so refreshing or unlocking there would only undo
// artwork set by AURA or Kometa. With replaceLocked, artwork locked in Plex
// is unlocked first and Kometa's "Overlay" label is cleared, so the refresh
// can apply the new artwork and Kometa re-applies its overlays on top.
func (pc *plexClient) refresh(ctx context.Context, servers []plexServer, typ mediaType, tmdb string, replaceLocked bool) (refreshResult, error) {
	var res refreshResult
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
		refs := idx.items[string(typ)+":"+tmdb]
		if len(refs) == 0 && time.Since(idx.built) > 15*time.Minute {
			// Possibly added since the index was built: rebuild once and retry.
			if idx, err = pc.index(ctx, srv, 0); err == nil {
				refs = idx.items[string(typ)+":"+tmdb]
			}
		}
		var live map[string]bool // library agents read just now
		if len(refs) > 0 {
			if live, err = pc.customSections(ctx, srv); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		for _, ref := range refs {
			// The index can be hours old (you may have switched a library
			// since), so trust only the agent as Plex reports it right now.
			if !live[ref.Section] {
				res.Skipped = append(res.Skipped, ref.Library+" on "+srv.Name)
				continue
			}
			if replaceLocked {
				if err := pc.unlock(ctx, srv, typ, ref); err != nil {
					errs = append(errs, err)
					continue
				}
			}
			if err := pc.do(ctx, srv, http.MethodPut, "/library/metadata/"+url.PathEscape(ref.Key)+"/refresh", nil); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Asked = append(res.Asked, srv.Name)
		}
	}
	return res, errors.Join(errs...)
}

// customSections reads which libraries use an agent with Mediux Artwork in
// it right now.
func (pc *plexClient) customSections(ctx context.Context, srv plexServer) (map[string]bool, error) {
	sections, err := pc.sections(ctx, srv)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, d := range sections {
		out[d.Key] = d.Mediux
	}
	return out, nil
}

// plexSection is one library and whether its agent includes this provider.
type plexSection struct {
	Key, Type, Title, Agent string
	Group                   string // the agent's provider group, when Plex says
	Mediux                  bool
}

// sections reads a server's libraries. A library's "agent" field names only
// its agent's Primary (Plex Series, say), so an agent you built in Plex from
// Plex Series plus Mediux Artwork looks just like plain Plex Series there.
// The agent itself is a provider group, so the groups are read too.
func (pc *plexClient) sections(ctx context.Context, srv plexServer) ([]plexSection, error) {
	var raw struct {
		MediaContainer struct {
			Directory []map[string]json.RawMessage `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := pc.do(ctx, srv, http.MethodGet, "/library/sections", &raw); err != nil {
		return nil, err
	}
	groups := pc.mediuxGroups(ctx, srv)
	out := make([]plexSection, 0, len(raw.MediaContainer.Directory))
	for _, d := range raw.MediaContainer.Directory {
		s := plexSection{Key: rawString(d["key"]), Type: rawString(d["type"]), Title: rawString(d["title"]),
			Agent: rawString(d["agent"]), Group: rawString(d["metadataAgentProviderGroupId"])}
		if has, ok := groups[s.Group]; ok && s.Group != "" {
			s.Mediux = has
		} else {
			// No group to go by: a custom Primary can only be a provider
			// group someone built, so count it as one with Mediux.
			s.Mediux = !builtInAgent(s.Agent)
		}
		out = append(out, s)
	}
	return out, nil
}

func ourProvider(identifier string) bool {
	return identifier == movieIdentifier || identifier == tvIdentifier
}

// mediuxGroups maps each of the server's agents (provider groups) to whether
// it includes Mediux Artwork. It's nil when Plex can't list them.
func (pc *plexClient) mediuxGroups(ctx context.Context, srv plexServer) map[string]bool {
	var providers struct {
		MediaContainer struct {
			MetadataAgentProvider []map[string]json.RawMessage `json:"MetadataAgentProvider"`
		} `json:"MediaContainer"`
	}
	var groups struct {
		MediaContainer struct {
			MetadataAgentProviderGroup []map[string]json.RawMessage `json:"MetadataAgentProviderGroup"`
		} `json:"MediaContainer"`
	}
	if err := pc.do(ctx, srv, http.MethodGet, "/media/providers/metadata", &providers); err != nil {
		return nil
	}
	if err := pc.do(ctx, srv, http.MethodGet, "/media/providers/metadata/group", &groups); err != nil {
		return nil
	}
	ours := map[string]bool{}
	for _, p := range providers.MediaContainer.MetadataAgentProvider {
		if ourProvider(rawString(p["identifier"])) {
			ours[rawString(p["id"])] = true
		}
	}
	out := map[string]bool{}
	for _, g := range groups.MediaContainer.MetadataAgentProviderGroup {
		has := ourProvider(rawString(g["primaryIdentifier"]))
		// Plex's documentation uses both names for the list of providers.
		for _, field := range []string{"MetadataAgentProviderGroupItem", "MetadataAgentGroupItem"} {
			var items []map[string]json.RawMessage
			_ = json.Unmarshal(g[field], &items)
			for _, it := range items {
				has = has || ours[rawString(it["metadataAgentProviderId"])]
			}
		}
		out[rawString(g["id"])] = has
	}
	return out
}

// rawString reads a JSON string or number as text: Plex sends IDs as either.
func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// Plex's numeric item types for library edits.
const (
	plexTypeMovie   = "1"
	plexTypeShow    = "2"
	plexTypeSeason  = "3"
	plexTypeEpisode = "4"
)

// unlock clears artwork locks and the Overlay label on a title, and for a
// show on all its seasons and episodes, using Plex's library edit call.
func (pc *plexClient) unlock(ctx context.Context, srv plexServer, typ mediaType, ref plexRef) error {
	edit := func(plexType string, ids []string, fields ...string) error {
		for len(ids) > 0 {
			n := min(len(ids), 100)
			q := url.Values{}
			q.Set("type", plexType)
			q.Set("id", strings.Join(ids[:n], ","))
			for _, f := range fields {
				q.Set(f+".locked", "0")
			}
			q.Set("label[].tag.tag-", "Overlay")
			path := "/library/sections/" + url.PathEscape(ref.Section) + "/all?" + q.Encode()
			if err := pc.do(ctx, srv, http.MethodPut, path, nil); err != nil {
				return err
			}
			ids = ids[n:]
		}
		return nil
	}
	if typ == typeMovie {
		return edit(plexTypeMovie, []string{ref.Key}, "thumb", "art")
	}
	if err := edit(plexTypeShow, []string{ref.Key}, "thumb", "art"); err != nil {
		return err
	}
	type children struct {
		MediaContainer struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	keys := func(path string) ([]string, error) {
		var out children
		if err := pc.do(ctx, srv, http.MethodGet, path, &out); err != nil {
			return nil, err
		}
		var ks []string
		for _, m := range out.MediaContainer.Metadata {
			ks = append(ks, m.RatingKey)
		}
		return ks, nil
	}
	seasons, err := keys("/library/metadata/" + url.PathEscape(ref.Key) + "/children")
	if err != nil {
		return err
	}
	if err := edit(plexTypeSeason, seasons, "thumb", "art"); err != nil {
		return err
	}
	episodes, err := keys("/library/metadata/" + url.PathEscape(ref.Key) + "/allLeaves")
	if err != nil {
		return err
	}
	return edit(plexTypeEpisode, episodes, "thumb")
}

// plexCurrent is what one Plex server shows for a title right now.
type plexCurrent struct {
	Server     string     `json:"server"`
	Title      string     `json:"title"`
	Poster     string     `json:"poster,omitempty"`
	Background string     `json:"background,omitempty"`
	Locked     bool       `json:"locked"`  // poster locked: uploaded by AURA, Kometa or by hand
	Overlay    bool       `json:"overlay"` // carries Kometa's Overlay label
	Seasons    []setImage `json:"seasons,omitempty"`
	Library    string     `json:"library,omitempty"`
	Custom     bool       `json:"custom"` // library uses a custom agent (Mediux can apply)
	Error      string     `json:"error,omitempty"`
}

type plexItem struct {
	Title string `json:"title"`
	Index int    `json:"index"`
	Thumb string `json:"thumb"`
	Art   string `json:"art"`
	Field []struct {
		Name   string `json:"name"`
		Locked bool   `json:"locked"`
	} `json:"Field"`
	Label []struct {
		Tag string `json:"tag"`
	} `json:"Label"`
}

// current reads the title's artwork from every configured server. Image
// addresses point back at this container (/api/plex-image), which adds the
// Plex token, so the token never reaches the browser.
func (pc *plexClient) current(ctx context.Context, servers []plexServer, typ mediaType, tmdb string) []plexCurrent {
	var out []plexCurrent
	for i, srv := range servers {
		if srv.Token == "" {
			continue
		}
		idx, err := pc.index(ctx, srv, 6*time.Hour)
		if err != nil {
			out = append(out, plexCurrent{Server: srv.Name, Error: err.Error()})
			continue
		}
		refs := append([]plexRef(nil), idx.items[string(typ)+":"+tmdb]...)
		// The index can be hours old: go by each library's agent right now.
		if live, err := pc.customSections(ctx, srv); err == nil {
			for i := range refs {
				refs[i].Custom = live[refs[i].Section]
			}
		}
		// Every library holding the title, Mediux-agent libraries first.
		sort.SliceStable(refs, func(a, b int) bool { return refs[a].Custom && !refs[b].Custom })
		for _, ref := range refs {
			out = append(out, pc.currentIn(ctx, i, srv, typ, ref))
		}
	}
	return out
}

// currentIn reads the artwork one library shows for a title.
func (pc *plexClient) currentIn(ctx context.Context, i int, srv plexServer, typ mediaType, ref plexRef) plexCurrent {
	cur := plexCurrent{Server: srv.Name, Library: ref.Library, Custom: ref.Custom}
	var meta struct {
		MediaContainer struct {
			Metadata []plexItem `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := pc.do(ctx, srv, http.MethodGet, "/library/metadata/"+url.PathEscape(ref.Key), &meta); err != nil || len(meta.MediaContainer.Metadata) == 0 {
		if err != nil {
			cur.Error = err.Error()
		}
		return cur
	}
	m := meta.MediaContainer.Metadata[0]
	cur.Title = m.Title
	cur.Poster = plexImageURL(i, m.Thumb, 300, 450)
	cur.Background = plexImageURL(i, m.Art, 480, 270)
	for _, f := range m.Field {
		if f.Name == "thumb" && f.Locked {
			cur.Locked = true
		}
	}
	for _, l := range m.Label {
		if l.Tag == "Overlay" {
			cur.Overlay = true
		}
	}
	if typ == typeShow {
		var kids struct {
			MediaContainer struct {
				Metadata []plexItem `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := pc.do(ctx, srv, http.MethodGet, "/library/metadata/"+url.PathEscape(ref.Key)+"/children", &kids); err == nil {
			for _, k := range kids.MediaContainer.Metadata {
				if k.Thumb != "" {
					cur.Seasons = append(cur.Seasons, setImage{Season: k.Index, URL: plexImageURL(i, k.Thumb, 150, 225)})
				}
			}
		}
	}
	return cur
}

func plexImageURL(server int, path string, w, h int) string {
	if path == "" {
		return ""
	}
	return fmt.Sprintf("/api/plex-image?s=%d&w=%d&h=%d&p=%s", server, w, h, url.QueryEscape(path))
}

// image fetches a resized image from a Plex server through its photo
// transcoder. Only library artwork paths are allowed.
func (pc *plexClient) image(ctx context.Context, srv plexServer, path string, w, h int) (*http.Response, error) {
	if !strings.HasPrefix(path, "/library/metadata/") || strings.Contains(path, "..") {
		return nil, errors.New("not a library image")
	}
	q := url.Values{}
	q.Set("width", fmt.Sprint(w))
	q.Set("height", fmt.Sprint(h))
	q.Set("minSize", "1")
	q.Set("upscale", "1")
	q.Set("url", path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(srv.URL, "/")+"/photo/:/transcode?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Plex-Token", srv.Token)
	return pc.http.Do(req)
}
