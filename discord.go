package main

// Discord notifications through a channel webhook. Messages are queued and
// sent in batches, so a large library scan can't trip Discord's rate limits.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color,omitempty"`
	Thumbnail   *discordImage  `json:"thumbnail,omitempty"`
	Footer      *discordFooter `json:"footer,omitempty"`
}

type discordImage struct {
	URL string `json:"url"`
}

type discordFooter struct {
	Text string `json:"text"`
}

const (
	colorNew     = 0x8B7CF6
	colorUpdated = 0x34D399
	discordBatch = 10 // Discord allows up to 10 embeds per message
	discordQueue = 60 // beyond this, extra items become a count in one summary
)

type discord struct {
	settings *settingsStore
	http     *http.Client

	mu      sync.Mutex
	queue   []discordEmbed
	dropped int
}

func newDiscord(store *settingsStore) *discord {
	return &discord{settings: store, http: &http.Client{Timeout: 15 * time.Second}}
}

func (d *discord) add(e discordEmbed) {
	if d.settings.get().DiscordWebhook == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) >= discordQueue {
		d.dropped++
		return
	}
	d.queue = append(d.queue, e)
}

// run sends queued notifications every 30 seconds.
func (d *discord) run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.flush(ctx)
		}
	}
}

func (d *discord) flush(ctx context.Context) {
	d.mu.Lock()
	n := min(len(d.queue), discordBatch)
	batch := append([]discordEmbed(nil), d.queue[:n]...)
	d.queue = d.queue[n:]
	dropped := 0
	if len(d.queue) == 0 && n < discordBatch { // room left for the summary
		dropped, d.dropped = d.dropped, 0
	}
	d.mu.Unlock()
	if dropped > 0 {
		batch = append(batch, discordEmbed{Title: fmt.Sprintf("…and %d more", dropped),
			Description: "More artwork changes than fit in a message. See the Log tab on the settings page."})
	}
	if len(batch) == 0 {
		return
	}
	if err := d.send(ctx, d.settings.get().DiscordWebhook, batch); err != nil {
		log.Printf("discord: %v", err)
	}
}

func (d *discord) send(ctx context.Context, webhook string, embeds []discordEmbed) error {
	if webhook == "" {
		return fmt.Errorf("no Discord webhook saved")
	}
	body, _ := json.Marshal(map[string]any{"username": "Mediux for Plex", "embeds": embeds})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("sending to Discord: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return fmt.Errorf("Discord answered %s: %s", res.Status, bytes.TrimSpace(msg))
	}
	return nil
}
