package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
	"html/template"
	"io"
	"net/http"
	"sync"
	"time"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Hub struct {
	config        config.Hub
	store         *store.Store
	tokens        map[string][32]byte
	templates     *template.Template
	logs          logQueue
	slots         chan struct{}
	uiSlots       chan struct{}
	eventSlots    chan struct{}
	subscribers   map[chan string]struct{}
	mu            sync.Mutex
	degraded      string
	loginWindow   time.Time
	loginAttempts int
}

func New(c config.Hub, s *store.Store) (*Hub, error) {
	if err := c.AlertPolicy.Validate(); err != nil {
		return nil, err
	}
	h := &Hub{config: c, store: s, tokens: map[string][32]byte{}, slots: make(chan struct{}, c.MaxRequests), uiSlots: make(chan struct{}, c.MaxRequests), eventSlots: make(chan struct{}, c.MaxRequests), subscribers: map[chan string]struct{}{}}
	if err := s.Configure(context.Background(), c.BucketSeconds); err != nil {
		return nil, err
	}
	for id, path := range c.Agents {
		secret, err := config.ReadSecret(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(secret))
		for _, other := range h.tokens {
			if sum == other {
				return nil, errors.New("agent credentials must be unique")
			}
		}
		h.tokens[id] = sum
	}
	var err error
	h.templates, err = template.New("").Funcs(template.FuncMap{"number": number, "reading": reading, "bytes": byteSize, "metricReading": h.metricReading, "containerState": h.containerState, "containerDetail": containerDetail, "resourcePolicy": h.resourcePolicyJSON, "stamp": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if c.AuthMode == "local" {
		if err = h.initLocal(); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(c.Agents))
	for id := range c.Agents {
		ids = append(ids, id)
	}
	if err = s.Register(context.Background(), ids); err != nil {
		h.setError("Could not register configured agents; check database storage.")
	}
	return h, nil
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(b[:])
}
func hashToken(s string) string        { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func (h *Hub) setError(message string) { h.mu.Lock(); h.degraded = message; h.mu.Unlock() }
func (h *Hub) Maintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.store.Maintain(ctx, time.Now(), time.Duration(h.config.RawHours)*time.Hour, time.Duration(h.config.SummaryDays)*24*time.Hour, h.config.StorageBytes); err != nil {
				h.setError("Storage maintenance failed; check disk capacity and database health.")
			}
		}
	}
}

func (h *Hub) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/samples", h.ingest)
	mux.HandleFunc("/v1/metrics", h.ingestOTLP)
	mux.HandleFunc("POST /v1/state", h.ingestState)
	mux.HandleFunc("GET /v1/logs/next", h.nextLog)
	mux.HandleFunc("POST /v1/logs/result", h.logResult)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Host-ID")
		expected, ok := h.tokens[id]
		actual := sha256.Sum256([]byte(bearer(r)))
		if !ok || subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			if r.URL.Path == "/v1/metrics" {
				writeOTLPError(w, r, 401, "unauthorized")
				return
			}
			http.Error(w, "unauthorized", 401)
			return
		}
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		default:
			w.Header().Set("Retry-After", "5")
			if r.URL.Path == "/v1/metrics" {
				writeOTLPError(w, r, 503, "hub busy")
				return
			}
			http.Error(w, "hub busy", 503)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, h.config.MaxBodyBytes)
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) < 7 || v[:7] != "Bearer " {
		return ""
	}
	return v[7:]
}
func decodeJSON(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("one JSON object required")
	}
	return nil
}
func (h *Hub) ingest(w http.ResponseWriter, r *http.Request) {
	var sample protocol.Sample
	if err := decodeJSON(r, &sample); err != nil {
		http.Error(w, "invalid sample JSON", 400)
		return
	}
	now := time.Now()
	if err := sample.Validate(r.Header.Get("X-Host-ID"), now, time.Duration(h.config.RawHours)*time.Hour, time.Duration(h.config.FutureSeconds)*time.Second, h.config.MaxEntities); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := h.store.Ingest(ctx, sample, now, h.config.BucketSeconds); err != nil {
		h.setError("Metrics could not be committed; check database and disk capacity.")
		http.Error(w, "storage unavailable", 503)
		return
	}
	h.notify(sample.HostID)
	w.WriteHeader(204)
}

func (h *Hub) UIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.fleet)
	mux.HandleFunc("GET /events", h.events)
	mux.HandleFunc("GET /hosts/{id}", h.host)
	mux.HandleFunc("POST /hosts/{id}/logs/{container}", h.showLogs)
	mux.HandleFunc("GET /login", h.loginPage)
	mux.HandleFunc("POST /login", h.login)
	mux.HandleFunc("GET /password", h.passwordPage)
	mux.HandleFunc("POST /password", h.password)
	mux.HandleFunc("POST /logout", h.logout)
	public := http.NewServeMux()
	public.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	public.HandleFunc("GET /static/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		switch name {
		case "style.css":
			w.Header().Set("Content-Type", "text/css")
		case "keiryo.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
		case "keiryo-32.png", "keiryo-512.png":
			w.Header().Set("Content-Type", "image/png")
		case "htmx.min.js", "dashboard.js", "echarts.min.js":
			w.Header().Set("Content-Type", "text/javascript")
		case "echarts.LICENSE.txt", "echarts.NOTICE.txt", "echarts.LICENSE-d3.txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		default:
			http.NotFound(w, r)
			return
		}
		b, err := assets.ReadFile("static/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	public.Handle("/", h.browserAuth(mux))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		// Long-lived event connections have their own bound; they cannot exhaust page slots.
		if r.URL.Path != "/events" {
			select {
			case h.uiSlots <- struct{}{}:
				defer func() { <-h.uiSlots }()
			default:
				http.Error(w, "hub busy", 503)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		public.ServeHTTP(w, r)
	})
}
