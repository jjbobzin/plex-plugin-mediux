package main

// Update check for the page footer. It reads which commit the published
// "latest" container image was built from (an image label), so "update
// available" only appears once a newer image actually exists, not for code
// that hasn't finished building.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	repoName     = "jjbobzin/plex-plugin-mediux"
	repoURL      = "https://github.com/" + repoName
	updateEvery  = 6 * time.Hour
	updateFailed = 30 * time.Minute
)

type latestImage struct {
	Revision string    `json:"revision"`
	Created  time.Time `json:"created"`
}

type updateChecker struct {
	registry string // https://ghcr.io, swapped in tests
	http     *http.Client

	mu      sync.Mutex
	latest  *latestImage
	checked time.Time
	err     string
}

func newUpdateChecker() *updateChecker {
	return &updateChecker{registry: "https://ghcr.io", http: &http.Client{Timeout: 20 * time.Second}}
}

func (u *updateChecker) get(ctx context.Context, url, accept, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := u.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("image registry answered %s", res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}

// fetch reads the revision label of the published "latest" image.
func (u *updateChecker) fetch(ctx context.Context) (*latestImage, error) {
	var tok struct {
		Token string `json:"token"`
	}
	if err := u.get(ctx, u.registry+"/token?scope=repository:"+repoName+":pull", "", "", &tok); err != nil {
		return nil, err
	}
	base := u.registry + "/v2/" + repoName
	var index struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := u.get(ctx, base+"/manifests/latest", "application/vnd.oci.image.index.v1+json", tok.Token, &index); err != nil {
		return nil, err
	}
	digest := ""
	for _, m := range index.Manifests {
		if m.Platform.OS == "linux" && m.Platform.Architecture == runtime.GOARCH {
			digest = m.Digest
		}
	}
	if digest == "" {
		return nil, fmt.Errorf("no %s image published", runtime.GOARCH)
	}
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := u.get(ctx, base+"/manifests/"+digest, "application/vnd.oci.image.manifest.v1+json", tok.Token, &manifest); err != nil {
		return nil, err
	}
	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := u.get(ctx, base+"/blobs/"+manifest.Config.Digest, "", tok.Token, &config); err != nil {
		return nil, err
	}
	labels := config.Config.Labels
	created, _ := time.Parse(time.RFC3339Nano, labels["org.opencontainers.image.created"])
	return &latestImage{Revision: labels["org.opencontainers.image.revision"], Created: created}, nil
}

// status returns the latest published image, checking at most every few hours.
func (u *updateChecker) status(ctx context.Context) (*latestImage, string) {
	u.mu.Lock()
	wait := updateEvery
	if u.err != "" {
		wait = updateFailed
	}
	if !u.checked.IsZero() && time.Since(u.checked) < wait {
		defer u.mu.Unlock()
		if u.err != "" {
			return nil, u.err // don't present an old answer as current
		}
		return u.latest, ""
	}
	u.mu.Unlock()
	latest, err := u.fetch(ctx)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checked = time.Now()
	if err != nil {
		u.err = err.Error()
		return nil, u.err
	}
	u.latest, u.err = latest, ""
	return u.latest, ""
}

// runningRevision is the commit this build came from, when it was built from
// main by the GitHub workflow ("main-<commit>").
func runningRevision() string {
	return strings.TrimPrefix(version, "main-")
}

// versionInfo answers GET /api/version for the page footer.
func (ui *webUI) versionInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	running := runningRevision()
	out := map[string]any{"version": version, "short": shortRev(running), "repo": repoURL}
	latest, problem := ui.updates.status(ctx)
	if problem != "" {
		out["check_error"] = problem
	}
	if latest != nil {
		out["latest"] = shortRev(latest.Revision)
		out["latest_built"] = latest.Created
		// Only builds from main can be compared; a local "dev" build can't.
		out["update_available"] = strings.HasPrefix(version, "main-") && latest.Revision != "" && latest.Revision != running
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}

func shortRev(rev string) string {
	if len(rev) > 7 && !strings.ContainsAny(rev, ".-") {
		return rev[:7]
	}
	return rev
}
