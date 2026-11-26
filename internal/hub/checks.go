package hub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sort"
	"time"

	"houselab/monitoring/internal/config"
)

// checksScope keys HTTP check conditions in the tracker; "@" cannot start a host ID.
const checksScope = "@checks"

type checkResult struct {
	At         time.Time
	OK         bool
	Status     int
	Latency    time.Duration
	CertExpiry time.Time
	Error      string
}

func checkClient(c config.HTTPCheck, transport http.RoundTripper) *http.Client {
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DisableKeepAlives = true
		transport = t
	}
	client := &http.Client{Timeout: time.Duration(c.TimeoutSeconds) * time.Second, Transport: transport}
	if !c.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client
}

// probe performs one check. Errors are generic so URLs (which may carry
// tokens) never reach the dashboard, logs or Discord.
func probe(ctx context.Context, client *http.Client, c config.HTTPCheck) checkResult {
	started := time.Now()
	out := checkResult{At: started}
	req, err := http.NewRequestWithContext(ctx, "GET", c.URL, nil)
	if err != nil {
		out.Error = "invalid request"
		return out
	}
	req.Header.Set("User-Agent", "keiryo-check")
	response, err := client.Do(req)
	out.Latency = time.Since(started)
	if err != nil {
		var verify *tls.CertificateVerificationError
		var unknown x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var invalid x509.CertificateInvalidError
		var netErr net.Error
		switch {
		case errors.As(err, &verify) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid):
			out.Error = "TLS verification failed"
		case errors.As(err, &netErr) && netErr.Timeout():
			out.Error = "timed out"
		default:
			out.Error = "connection failed"
		}
		return out
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	out.Status = response.StatusCode
	if response.TLS != nil && len(response.TLS.PeerCertificates) > 0 {
		out.CertExpiry = response.TLS.PeerCertificates[0].NotAfter
	}
	if len(c.ExpectStatus) > 0 {
		out.OK = slices.Contains(c.ExpectStatus, out.Status)
	} else {
		out.OK = out.Status >= 200 && out.Status < 400
	}
	if !out.OK {
		out.Error = fmt.Sprintf("unexpected status %d", out.Status)
	}
	return out
}

func (h *Hub) recordCheck(name string, r checkResult) {
	h.mu.Lock()
	previous, seen := h.checkResults[name]
	h.checkResults[name] = r
	h.mu.Unlock()
	if !seen || previous.OK != r.OK {
		h.notify("")
	}
}

// Checks runs every configured probe on its own interval, starting immediately.
func (h *Hub) Checks(ctx context.Context) {
	for _, c := range h.config.HTTPChecks {
		go func(c config.HTTPCheck) {
			client := checkClient(c, nil)
			ticker := time.NewTicker(time.Duration(c.IntervalSeconds) * time.Second)
			defer ticker.Stop()
			for {
				h.recordCheck(c.Name, probe(ctx, client, c))
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}(c)
	}
}

// checkSignals turns the latest results into conditions. A check without a
// result or with a result older than three intervals reports nothing new;
// certificate conditions only exist for successful HTTPS responses.
func (h *Hub) checkSignals() []condition {
	h.mu.Lock()
	results := make(map[string]checkResult, len(h.checkResults))
	for k, v := range h.checkResults {
		results[k] = v
	}
	h.mu.Unlock()
	var out []condition
	for _, c := range h.config.HTTPChecks {
		r, ok := results[c.Name]
		if !ok || time.Since(r.At) > 3*time.Duration(c.IntervalSeconds)*time.Second {
			continue
		}
		link := "/?view=conditions#checks"
		ms := r.Latency.Milliseconds()
		if r.OK {
			out = append(out, condition{Key: "check/" + c.Name, Kind: "HTTP check", Name: c.Name, Link: link, State: "healthy", Detail: fmt.Sprintf("%s: %d in %d ms", c.Name, r.Status, ms)})
		} else {
			out = append(out, condition{Key: "check/" + c.Name, Kind: "HTTP check", Name: c.Name, Link: link, State: c.Severity, Detail: fmt.Sprintf("%s: %s (%d ms)", c.Name, r.Error, ms)})
		}
		if r.CertExpiry.IsZero() || c.TLSWarnDays == 0 && c.TLSCriticalDays == 0 {
			continue
		}
		days := time.Until(r.CertExpiry).Hours() / 24
		state := "healthy"
		switch {
		case c.TLSCriticalDays > 0 && days < float64(c.TLSCriticalDays):
			state = "critical"
		case c.TLSWarnDays > 0 && days < float64(c.TLSWarnDays):
			state = "warning"
		}
		out = append(out, condition{Key: "cert/" + c.Name, Kind: "TLS certificate", Name: c.Name, Link: link, State: state, Detail: fmt.Sprintf("%s certificate expires in %.1f days", c.Name, days)})
	}
	return out
}

// displayChecks applies the same pending rule as hosts.
func (h *Hub) displayChecks(ctx context.Context, now time.Time) []condition {
	signals := h.checkSignals()
	if h.config.AlertPendingSeconds == 0 || len(signals) == 0 {
		return signals
	}
	states, err := h.store.Notifications(ctx, checksScope)
	if err != nil {
		return signals
	}
	for i, s := range signals {
		tracked, exists := states[s.Key]
		signals[i] = h.pendingSignal(s, tracked, exists, now)
	}
	return signals
}

type checkView struct {
	Name, State string
	Details     []string
	At          time.Time
}

// checkViews groups displayed conditions per configured check, worst state first.
func (h *Hub) checkViews(signals []condition) []checkView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]checkView, 0, len(h.config.HTTPChecks))
	for _, c := range h.config.HTTPChecks {
		v := checkView{Name: c.Name, State: "unavailable", At: h.checkResults[c.Name].At}
		for _, s := range signals {
			if s.Name != c.Name {
				continue
			}
			if v.State == "unavailable" || severity(s.State) > severity(v.State) {
				v.State = s.State
			}
			v.Details = append(v.Details, s.Detail)
		}
		if len(v.Details) == 0 {
			v.Details = []string{"No recent result"}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return severity(out[i].State) > severity(out[j].State) })
	return out
}
