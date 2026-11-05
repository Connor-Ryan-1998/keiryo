package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/store"
)

func discordWebhook(path string) (string, error) {
	secret, err := config.ReadSecret(path)
	if err != nil {
		return "", errors.New("cannot read restricted Discord webhook file")
	}
	u, err := url.Parse(secret)
	if err != nil {
		return "", errors.New("invalid Discord webhook URL")
	}
	parts := strings.Split(u.Path, "/")
	if u.Scheme != "https" || (u.Host != "discord.com" && u.Host != "discordapp.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(parts) != 5 || parts[1] != "api" || parts[2] != "webhooks" || parts[3] == "" || parts[4] == "" {
		return "", errors.New("Discord webhook requires an HTTPS discord.com or discordapp.com webhook URL")
	}
	return secret, nil
}

// Notifications runs independently of browsers and ingestion; slow webhooks cannot block either.
func (h *Hub) Notifications(ctx context.Context) {
	if h.config.DiscordWebhookFile == "" {
		return
	}
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			webhook, err := discordWebhook(h.config.DiscordWebhookFile)
			if err != nil {
				h.setError("Discord notification secret is unavailable or invalid.")
				continue
			}
			pass, cancel := context.WithTimeout(ctx, 20*time.Second)
			ids := make([]string, 0, len(h.config.Agents))
			for id := range h.config.Agents {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				host, e := h.store.Host(pass, id)
				if e == nil {
					e = h.reconcileNotifications(pass, host, time.Now(), client, webhook)
				}
				if e != nil {
					h.setError("Alert delivery failed; check Discord configuration, connectivity and database health.")
					break
				}
			}
			cancel()
		}
	}
}

func (h *Hub) reconcileNotifications(ctx context.Context, host store.Host, now time.Time, client *http.Client, webhook string) error {
	states, err := h.store.Notifications(ctx, host.ID)
	if err != nil {
		return err
	}
	row := h.fleetRow(host)
	signals := map[string]condition{}
	for _, signal := range row.Signals {
		signals[signal.Key] = signal
	}
	// A fresh complete state snapshot can clear collection errors, but absence of a
	// container or missing metrics must never be interpreted as a healthy service.
	if h.stateFresh(host.Sample) {
		for key := range states {
			if strings.HasPrefix(key, "collection/") {
				if _, exists := signals[key]; !exists {
					signals[key] = condition{Key: key, Kind: "Collection", State: "healthy", Detail: "Collection restored"}
				}
			}
		}
	}
	for key, signal := range signals {
		old, exists := states[key]
		if !exists && signal.State == "healthy" {
			continue
		}
		if !exists || old.State != signal.State {
			old.State, old.Since = signal.State, now.Unix()
			// Preserve delivery cooldowns when a flapping condition changes state.
		}
		old.Kind, old.Detail = signal.Kind, signal.Detail
		if old.State == "healthy" && old.Delivered == "" {
			delete(states, key)
		} else {
			states[key] = old
		}
	}
	if err = h.store.SaveNotifications(ctx, host.ID, states); err != nil {
		return err
	}
	retryAt, err := h.store.NotificationRetryAt(ctx)
	if err != nil {
		return err
	}
	if now.Unix() < retryAt {
		return nil
	}
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sent := 0
	for _, key := range keys {
		n := states[key]
		if _, observed := signals[key]; !observed {
			continue
		}
		if n.State == n.Delivered || now.Unix() < n.Next || now.Unix()-n.Since < int64(h.config.AlertPendingSeconds) {
			continue
		}
		label := strings.ToUpper(n.State)
		if n.State == "healthy" {
			label = "RECOVERED"
		}
		message := fmt.Sprintf("[%s] %s (%s) · %s\n%s\n%s", label, hostName(host), host.ID, n.Kind, n.Detail, h.config.PublicURL+"/hosts/"+url.PathEscape(host.ID))
		delay, sendErr := sendDiscord(ctx, client, webhook, message, n.Attempts)
		if sendErr != nil {
			n.Attempts = min(n.Attempts+1, 10)
			n.Next = now.Add(delay).Unix()
			states[key] = n
			if err = h.store.SaveNotifications(ctx, host.ID, states); err != nil {
				return err
			}
			if err = h.store.SetNotificationRetryAt(ctx, n.Next); err != nil {
				return err
			}
			return sendErr
		}
		n.Delivered, n.Attempts, n.Next = n.State, 0, 0
		if n.State == "healthy" {
			delete(states, key)
		} else {
			states[key] = n
		}
		if err = h.store.SaveNotifications(ctx, host.ID, states); err != nil {
			return err
		}
		sent++
		if sent >= 20 {
			break
		}
	}
	return nil
}

func sendDiscord(ctx context.Context, client *http.Client, webhook, message string, attempts int) (time.Duration, error) {
	// Discord limits content to 2000 characters. Mentions are always disabled.
	runes := []rune(message)
	if len(runes) > 1900 {
		message = string(runes[:1900]) + "…"
	}
	body, err := json.Marshal(struct {
		Content         string `json:"content"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}{Content: message, AllowedMentions: struct {
		Parse []string `json:"parse"`
	}{Parse: []string{}}})
	if err != nil {
		return time.Hour, errors.New("cannot encode Discord notification")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", webhook, bytes.NewReader(body))
	if err != nil {
		return time.Hour, errors.New("cannot create Discord notification")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	backoff := time.Duration(min(3600, 30*(1<<min(attempts, 7)))) * time.Second
	if err != nil {
		return backoff, errors.New("Discord notification request failed")
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return 0, nil
	}
	if response.StatusCode == 429 {
		seconds, e := strconv.ParseFloat(response.Header.Get("Retry-After"), 64)
		if e == nil && seconds >= 1 && seconds <= 86400 {
			return time.Duration(seconds * float64(time.Second)), errors.New("Discord notification rate limited")
		}
		return time.Minute, errors.New("Discord notification rate limited")
	}
	if response.StatusCode < 500 {
		return time.Hour, errors.New("Discord rejected notification; check webhook configuration")
	}
	return backoff, errors.New("Discord notification service unavailable")
}
