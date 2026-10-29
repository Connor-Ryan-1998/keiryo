package hub

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"golang.org/x/crypto/bcrypt"
	"houselab/monitoring/internal/config"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"
)

type authKey struct{}
type browserSession struct{ ID, CSRF string }

func (h *Hub) initLocal() error {
	ctx := context.Background()
	initialized, err := h.store.Setting(ctx, "initialized")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if initialized != "yes" {
		listen, _, listenErr := net.SplitHostPort(h.config.UIListen)
		origin, originErr := url.Parse(h.config.PublicURL)
		if listenErr != nil || originErr != nil || !config.Loopback(listen) || !config.Loopback(origin.Hostname()) {
			return errors.New("initialize admin/admin using loopback UI binding and public_url before enabling remote access")
		}
	}
	_, err = h.store.Setting(ctx, "password")
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return h.store.SetSetting(ctx, "password", string(hash))
}
func (h *Hub) trusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, s := range h.config.TrustedProxies {
		p, err := netip.ParsePrefix(s)
		if err == nil && p.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}
func (h *Hub) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || strings.TrimRight(origin, "/") == strings.TrimRight(h.config.PublicURL, "/")
}
func (h *Hub) browserAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if h.config.AuthMode == "proxy" {
			if !h.trusted(r) || r.Header.Get("X-authentik-username") == "" || r.URL.Path == "/login" || r.URL.Path == "/password" {
				http.Error(w, "trusted authenticated proxy required", 403)
				return
			}
			// Authentik authenticates the user; an exact same-origin token protects browser mutations.
			if r.Method != "GET" && r.Method != "HEAD" {
				if !h.originOK(r) || r.Header.Get("Origin") == "" {
					http.Error(w, "same-origin request required", 403)
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie("session")
		if err != nil || len(cookie.Value) != 64 {
			http.Redirect(w, r, "/login", 303)
			return
		}
		csrf, err := h.store.Session(r.Context(), hashToken(cookie.Value), time.Now())
		if err != nil {
			http.Redirect(w, r, "/login", 303)
			return
		}
		session := browserSession{ID: hashToken(cookie.Value), CSRF: csrf}
		if r.Method != "GET" && r.Method != "HEAD" {
			if !h.originOK(r) || r.ParseForm() != nil || subtle.ConstantTimeCompare([]byte(r.Form.Get("csrf")), []byte(csrf)) != 1 {
				http.Error(w, "invalid CSRF token", 403)
				return
			}
		}
		initialized, err := h.store.Setting(r.Context(), "initialized")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "storage unavailable", 503)
			return
		}
		if initialized != "yes" && r.URL.Path != "/password" && r.URL.Path != "/logout" {
			http.Redirect(w, r, "/password", 303)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey{}, session)))
	})
}
func (h *Hub) cookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: strings.HasPrefix(h.config.PublicURL, "https://"), SameSite: http.SameSiteStrictMode})
}
func (h *Hub) loginPage(w http.ResponseWriter, r *http.Request) {
	csrf := randomToken()
	h.cookie(w, "login_csrf", csrf, 600)
	h.render(w, "login", map[string]any{"CSRF": csrf})
}
func (h *Hub) login(w http.ResponseWriter, r *http.Request) {
	if !h.originOK(r) || r.ParseForm() != nil {
		http.Error(w, "invalid request", 403)
		return
	}
	cookie, err := r.Cookie("login_csrf")
	if err != nil || len(cookie.Value) != 64 || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.Form.Get("csrf"))) != 1 {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	h.mu.Lock()
	if time.Since(h.loginWindow) > time.Minute {
		h.loginWindow = time.Now()
		h.loginAttempts = 0
	}
	h.loginAttempts++
	limited := h.loginAttempts > 10
	h.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many login attempts", 429)
		return
	}
	hash, err := h.store.Setting(r.Context(), "password")
	if err != nil {
		http.Error(w, "storage unavailable", 503)
		return
	}
	password := r.Form.Get("password")
	valid := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	if !valid || r.Form.Get("username") != "admin" {
		http.Error(w, "invalid credentials", 401)
		return
	}
	initialized, _ := h.store.Setting(r.Context(), "initialized")
	if initialized != "yes" {
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(remote)
		if !config.Loopback(remote) && (ip == nil || !ip.IsPrivate()) && !h.trusted(r) {
			http.Error(w, "bootstrap requires private access", 403)
			return
		}
	}
	id, csrf := randomToken(), randomToken()
	if err = h.store.PutSession(r.Context(), hashToken(id), csrf, time.Now().Add(12*time.Hour)); err != nil {
		http.Error(w, "storage unavailable", 503)
		return
	}
	h.cookie(w, "session", id, 43200)
	h.cookie(w, "login_csrf", "", -1)
	http.Redirect(w, r, "/", 303)
}
func (h *Hub) passwordPage(w http.ResponseWriter, r *http.Request) {
	session, _ := r.Context().Value(authKey{}).(browserSession)
	h.render(w, "password", map[string]any{"CSRF": session.CSRF})
}
func (h *Hub) password(w http.ResponseWriter, r *http.Request) {
	password := r.Form.Get("password")
	if len(password) < 6 || len(password) > 72 || !strings.ContainsFunc(password, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }) {
		http.Error(w, "password must contain 6–72 bytes and at least one special character (punctuation or symbol)", 400)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "password hashing failed", 500)
		return
	}
	if err = h.store.ChangePassword(r.Context(), string(hash)); err != nil {
		http.Error(w, "storage unavailable", 503)
		return
	}
	h.cookie(w, "session", "", -1)
	http.Redirect(w, r, "/login", 303)
}
func (h *Hub) logout(w http.ResponseWriter, r *http.Request) {
	if session, ok := r.Context().Value(authKey{}).(browserSession); ok {
		if err := h.store.DeleteSession(r.Context(), session.ID); err != nil {
			http.Error(w, "storage unavailable", 503)
			return
		}
	}
	h.cookie(w, "session", "", -1)
	http.Redirect(w, r, "/login", 303)
}

func (h *Hub) csrf(r *http.Request) string {
	session, _ := r.Context().Value(authKey{}).(browserSession)
	return session.CSRF
}
