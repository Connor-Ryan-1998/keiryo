package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

type buffer struct {
	max     int
	samples []protocol.Sample
	dropped int
}

func (b *buffer) add(s protocol.Sample) {
	if len(b.samples) == b.max {
		b.samples = b.samples[1:]
		b.dropped++
	}
	b.samples = append(b.samples, s)
}

type Agent struct {
	Config    config.Agent
	token     string
	client    *http.Client
	collector *Collector
}

var errRejected = errors.New("hub permanently rejected sample")

func New(c config.Agent) (*Agent, error) {
	token, err := config.ReadSecret(c.CredentialFile)
	if err != nil {
		return nil, err
	}
	return &Agent{Config: c, token: token, client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, collector: NewCollector(c)}, nil
}
func (a *Agent) request(ctx context.Context, method, path string, v any) (*http.Response, error) {
	var body io.Reader
	if v != nil {
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Config.HubURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("X-Host-ID", a.Config.HostID)
	req.Header.Set("Content-Type", "application/json")
	return a.client.Do(req)
}
func (a *Agent) uploadState(ctx context.Context, s protocol.AgentState) error {
	res, err := a.request(ctx, "POST", "/v1/state", s)
	if err != nil {
		return errors.New("hub upload unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 429 {
		return errRejected
	}
	if res.StatusCode != 204 {
		return errors.New("hub rejected sample")
	}
	return nil
}
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	metrics, state := make(chan protocol.Sample, 1), make(chan protocol.Sample, 1)
	var workers sync.WaitGroup
	workers.Add(3)
	go func() { defer workers.Done(); a.deliver(ctx, metrics, "metrics", a.exportMetrics) }()
	go func() {
		defer workers.Done()
		a.deliver(ctx, state, "state", func(ctx context.Context, s protocol.Sample) error {
			snapshot := s.State()
			snapshot.LogFetch = a.collector.Docker != nil
			return a.uploadState(ctx, snapshot)
		})
	}()
	go func() { defer workers.Done(); a.pollLogs(ctx) }()
	defer func() { cancel(); workers.Wait() }()
	ticker := time.NewTicker(time.Duration(a.Config.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		collectCtx, stop := context.WithTimeout(ctx, min(50*time.Second, time.Duration(a.Config.IntervalSeconds)*time.Second))
		s := a.collector.Collect(collectCtx)
		stop()
		for _, ch := range []chan protocol.Sample{metrics, state} {
			select {
			case ch <- s:
			default:
				select {
				case <-ch:
					slog.Warn("agent delivery staging buffer overflow; oldest collection discarded")
				default:
				}
				select {
				case ch <- s:
				case <-ctx.Done():
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *Agent) deliver(ctx context.Context, samples <-chan protocol.Sample, kind string, send func(context.Context, protocol.Sample) error) {
	b := buffer{max: a.Config.BufferSamples}
	retry := time.NewTimer(time.Hour)
	defer retry.Stop()
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-samples:
			if !ok {
				return
			}
			if b.dropped > 0 {
				s.Errors = append(append([]string{}, s.Errors[:min(len(s.Errors), 63)]...), "agent discarded samples after overflow or permanent rejection")
				b.dropped = 0
			}
			empty := len(b.samples) == 0
			b.add(s)
			if empty {
				retry.Reset(time.Millisecond)
			}
		case <-retry.C:
			if len(b.samples) == 0 {
				continue
			}
			if err := send(ctx, b.samples[0]); err != nil {
				if errors.Is(err, errRejected) {
					b.samples = b.samples[1:]
					b.dropped++
					slog.Warn("destination permanently rejected sample; discarded", "kind", kind)
					backoff = time.Second
					retry.Reset(time.Millisecond)
					continue
				}
				slog.Warn("upload failed; retrying", "kind", kind)
				delay := backoff + time.Duration(rand.IntN(1000))*time.Millisecond
				var throttle *retryAfterError
				if errors.As(err, &throttle) {
					delay = max(delay, throttle.after)
				}
				retry.Reset(delay)
				backoff = min(backoff*2, time.Minute)
			} else {
				b.samples = b.samples[1:]
				backoff = time.Second
				retry.Reset(time.Millisecond)
			}
		}
	}
}
func (a *Agent) pollLogs(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(a.Config.PollSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			res, err := a.request(ctx, "GET", "/v1/logs/next", nil)
			if err != nil {
				continue
			}
			if res.StatusCode != 200 {
				res.Body.Close()
				continue
			}
			var req protocol.LogRequest
			err = json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&req)
			res.Body.Close()
			if err != nil || req.HostID != a.Config.HostID || !config.ValidID(req.ID) || !config.ValidID(req.ContainerID) {
				continue
			}
			deadline := min(req.Expires.UnixNano(), time.Now().Add(time.Duration(a.Config.LogTimeoutSeconds)*time.Second).UnixNano())
			logctx, cancel := context.WithDeadline(ctx, time.Unix(0, deadline))
			text, err := a.collector.Docker.Logs(logctx, req.ContainerID, min(max(req.Lines, 1), a.Config.LogLines), min(max(req.Bytes, 1), a.Config.LogBytes))
			cancel()
			out := protocol.LogResult{ID: req.ID, Text: text}
			if err != nil {
				out.Error = "container logs unavailable"
			}
			res, err = a.request(ctx, "POST", "/v1/logs/result", out)
			if err == nil {
				res.Body.Close()
			}
		}
	}
}
