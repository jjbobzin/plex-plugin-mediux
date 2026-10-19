package main

// "Sign in with Plex": the standard plex.tv PIN sign-in that Overseerr and
// Tautulli use. The page opens Plex's sign-in window; the provider then lists
// the account's servers with their access tokens and tests which address of
// each server this container can actually reach. Tokens stay on the server:
// the page only ever sees server names and addresses.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const plexProduct = "Mediux for Plex"

type plexAuth struct {
	settings *settingsStore
	http     *http.Client
	plexTV   string // https://plex.tv, swapped in tests
	// extraHosts are tried for every server besides the addresses plex.tv
	// knows, e.g. host.docker.internal for a Plex on host networking.
	extraHosts []string

	mu       sync.Mutex
	sessions map[string]*authSession
}

type authSession struct {
	pinID   int
	code    string
	created time.Time
	busy    bool // a check is already finding servers
	done    bool
	servers []foundServer
	err     string
}

// foundServer is one Plex server on the signed-in account.
type foundServer struct {
	Machine string `json:"machine"`
	Name    string `json:"name"`
	Owned   bool   `json:"owned"`
	URL     string `json:"url,omitempty"` // first address that answered as this server
	Note    string `json:"note,omitempty"`
	token   string
}

func newPlexAuth(store *settingsStore) *plexAuth {
	return &plexAuth{settings: store, http: &http.Client{Timeout: 15 * time.Second},
		plexTV: "https://plex.tv", extraHosts: []string{"host.docker.internal"}, sessions: map[string]*authSession{}}
}

// clientID is this install's stable Plex client identifier.
func (a *plexAuth) clientID() (string, error) {
	cfg := a.settings.get()
	if cfg.PlexClientID != "" {
		return cfg.PlexClientID, nil
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	cfg.PlexClientID = "mediux-for-plex-" + hex.EncodeToString(raw)
	if err := a.settings.save(cfg); err != nil {
		return "", err
	}
	return cfg.PlexClientID, nil
}

func (a *plexAuth) call(ctx context.Context, method, path, token string, out any) error {
	id, err := a.clientID()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, a.plexTV+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Product", plexProduct)
	req.Header.Set("X-Plex-Version", version)
	req.Header.Set("X-Plex-Client-Identifier", id)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	res, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching plex.tv: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("plex.tv answered %s", res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}

// start asks plex.tv for a sign-in PIN and returns the window address.
func (a *plexAuth) start(ctx context.Context) (string, string, error) {
	var pin struct {
		ID   int    `json:"id"`
		Code string `json:"code"`
	}
	if err := a.call(ctx, http.MethodPost, "/api/v2/pins?strong=true", "", &pin); err != nil {
		return "", "", err
	}
	if pin.ID == 0 || pin.Code == "" {
		return "", "", errors.New("plex.tv didn't return a sign-in code")
	}
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	session := hex.EncodeToString(raw)
	a.mu.Lock()
	for k, s := range a.sessions {
		if time.Since(s.created) > 30*time.Minute {
			delete(a.sessions, k)
		}
	}
	a.sessions[session] = &authSession{pinID: pin.ID, code: pin.Code, created: time.Now()}
	a.mu.Unlock()
	id, _ := a.clientID()
	q := url.Values{}
	q.Set("clientID", id)
	q.Set("code", pin.Code)
	q.Set("context[device][product]", plexProduct)
	return session, "https://app.plex.tv/auth#?" + q.Encode(), nil
}

// check reports whether sign-in finished; once it has, it lists the
// account's servers with a tested address for each.
func (a *plexAuth) check(ctx context.Context, session string) (bool, []foundServer, error) {
	a.mu.Lock()
	s := a.sessions[session]
	if s == nil {
		a.mu.Unlock()
		return false, nil, errors.New("this sign-in has expired; start again")
	}
	if s.done {
		servers, msg := s.servers, s.err
		a.mu.Unlock()
		if msg != "" {
			return true, nil, errors.New(msg)
		}
		return true, servers, nil
	}
	if s.busy {
		a.mu.Unlock()
		return false, nil, nil // another check is finding the servers
	}
	s.busy = true
	pinID := s.pinID
	a.mu.Unlock()
	finished := false
	defer func() {
		if !finished {
			a.mu.Lock()
			s.busy = false
			a.mu.Unlock()
		}
	}()
	var pin struct {
		AuthToken string `json:"authToken"`
	}
	if err := a.call(ctx, http.MethodGet, fmt.Sprintf("/api/v2/pins/%d", pinID), "", &pin); err != nil {
		return false, nil, err
	}
	if pin.AuthToken == "" {
		return false, nil, nil // still waiting for the person to sign in
	}
	// Finding servers can outlast this request (a proxy timeout, the page
	// giving up), so it runs on its own clock and the result is kept for
	// the next check.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	servers, err := a.discover(dctx, pin.AuthToken)
	cancel()
	a.mu.Lock()
	s.done, s.busy, s.servers = true, false, servers
	if err != nil {
		s.err = err.Error()
	}
	a.mu.Unlock()
	finished = true
	return true, servers, err
}

// discover lists the account's servers and finds a working address for each.
func (a *plexAuth) discover(ctx context.Context, userToken string) ([]foundServer, error) {
	var resources []struct {
		Name             string `json:"name"`
		Provides         string `json:"provides"`
		ClientIdentifier string `json:"clientIdentifier"`
		AccessToken      string `json:"accessToken"`
		Owned            bool   `json:"owned"`
		Connections      []struct {
			URI   string `json:"uri"`
			Port  int    `json:"port"`
			Local bool   `json:"local"`
			Relay bool   `json:"relay"`
		} `json:"connections"`
	}
	if err := a.call(ctx, http.MethodGet, "/api/v2/resources?includeHttps=1&includeRelay=0", userToken, &resources); err != nil {
		return nil, err
	}
	var found []foundServer
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, r := range resources {
		if !strings.Contains(r.Provides, "server") || r.AccessToken == "" {
			continue
		}
		var local, remote []string
		ports := map[int]bool{}
		for _, c := range r.Connections {
			if c.Relay || c.URI == "" {
				continue
			}
			if c.Local {
				local = append(local, c.URI)
				ports[c.Port] = true
			} else {
				remote = append(remote, c.URI)
			}
		}
		if len(ports) == 0 {
			ports[32400] = true
		}
		var extra []string
		for _, h := range a.extraHosts {
			for p := range ports {
				extra = append(extra, fmt.Sprintf("http://%s:%d", h, p))
			}
		}
		candidates := append(append(local, extra...), remote...)
		fs := foundServer{Machine: r.ClientIdentifier, Name: r.Name, Owned: r.Owned, token: r.AccessToken}
		wg.Add(1)
		go func() {
			defer wg.Done()
			fs.URL = a.firstReachable(ctx, candidates, fs.Machine)
			if fs.URL == "" {
				fs.Note = "None of its addresses answered from this container. You can still add it by hand."
			} else if isRemote(fs.URL, remote) {
				fs.Note = "Reached through its internet address; a local address would be faster."
			}
			mu.Lock()
			found = append(found, fs)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(found, func(i, j int) bool {
		if found[i].Owned != found[j].Owned {
			return found[i].Owned
		}
		return found[i].Name < found[j].Name
	})
	return found, nil
}

func isRemote(u string, remote []string) bool {
	for _, r := range remote {
		if r == u {
			return true
		}
	}
	return false
}

// firstReachable tries addresses in order and returns the first one where
// the server that answers is the expected one (same machine identifier), so
// a shared host address can't be mistaken for a different Plex server.
func (a *plexAuth) firstReachable(ctx context.Context, candidates []string, machine string) string {
	client := &http.Client{Timeout: 4 * time.Second}
	for _, base := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/identity", nil)
		if err != nil {
			continue
		}
		// /identity needs no token: the server's token is only ever sent to an
		// address after it has proved to be that server.
		req.Header.Set("Accept", "application/json")
		res, err := client.Do(req)
		if err != nil {
			continue
		}
		var id struct {
			MediaContainer struct {
				MachineIdentifier string `json:"machineIdentifier"`
			} `json:"MediaContainer"`
		}
		err = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&id)
		res.Body.Close()
		if err == nil && res.StatusCode == http.StatusOK && id.MediaContainer.MachineIdentifier == machine {
			return strings.TrimRight(base, "/")
		}
	}
	return ""
}

// add saves the chosen servers, replacing any saved server with the same name.
func (a *plexAuth) add(session string, machines []string) (int, error) {
	a.mu.Lock()
	s := a.sessions[session]
	var servers []foundServer
	ready := s != nil && s.done
	if ready {
		servers = s.servers
	}
	a.mu.Unlock()
	if !ready {
		return 0, errors.New("this sign-in has expired; start again")
	}
	want := map[string]bool{}
	for _, m := range machines {
		want[m] = true
	}
	cfg := a.settings.get()
	existing := len(cfg.PlexServers)
	names := map[string]bool{}
	for _, p := range cfg.PlexServers {
		names[p.Name] = true
	}
	added := 0
	for _, f := range servers {
		if !want[f.Machine] || f.URL == "" {
			continue
		}
		srv := plexServer{Name: f.Name, URL: f.URL, Token: f.token, Machine: f.Machine}
		// Update the saved entry for this same server (matched by its Plex
		// ID), or a saved entry of the same name that has no ID yet.
		replaced := false
		for i := 0; i < existing; i++ {
			p := &cfg.PlexServers[i]
			if p.Machine == f.Machine || p.Machine == "" && p.Name == f.Name {
				srv.Name = p.Name
				*p, replaced = srv, true
				break
			}
		}
		if !replaced {
			// Different servers can share a name: keep each one distinct.
			for n := 2; names[srv.Name]; n++ {
				srv.Name = fmt.Sprintf("%s (%d)", f.Name, n)
			}
			cfg.PlexServers = append(cfg.PlexServers, srv)
		}
		names[srv.Name] = true
		added++
	}
	if added == 0 {
		return 0, errors.New("pick at least one server that answered")
	}
	return added, a.settings.save(cfg)
}
