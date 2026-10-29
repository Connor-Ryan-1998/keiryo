package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
	"houselab/monitoring/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testHub(t *testing.T, mode string) *Hub {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte(strings.Repeat("s", 32)), 0600)
	c := config.DefaultHub()
	c.AuthMode = mode
	c.Agents = map[string]string{"node": token}
	if mode == "proxy" {
		c.TrustedProxies = []string{"10.0.0.1/32"}
		c.PublicURL = "https://monitor.example.com"
	}
	h, err := New(c, s)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func request(h http.Handler, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:40000"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestLocalBootstrapAndSessionBoundary(t *testing.T) {
	h := testHub(t, "local")
	ui := h.UIHandler()
	if w := request(ui, "GET", "/", "", nil); w.Code != 303 {
		t.Fatalf("anonymous access %d", w.Code)
	}
	login := request(ui, "GET", "/login", "", nil)
	cookies := login.Result().Cookies()
	var csrf string
	for _, c := range cookies {
		if c.Name == "login_csrf" {
			csrf = c.Value
		}
	}
	if csrf == "" {
		t.Fatal("login CSRF missing")
	}
	if icon := request(ui, "GET", "/favicon.ico", "", cookies); icon.Code != http.StatusNoContent || len(icon.Result().Cookies()) != 0 {
		t.Fatal("favicon must not redirect through login and replace its CSRF cookie")
	}
	for _, asset := range []string{"style.css", "htmx.min.js", "echarts.min.js", "echarts.LICENSE.txt", "echarts.NOTICE.txt", "echarts.LICENSE-d3.txt"} {
		w := request(ui, "GET", "/static/"+asset, "", cookies)
		if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 {
			t.Fatalf("login asset %s: status %d, cookies %v", asset, w.Code, w.Result().Cookies())
		}
	}
	if w := request(ui, "GET", "/static/unknown", "", cookies); w.Code != http.StatusNotFound {
		t.Fatalf("unknown asset status %d", w.Code)
	}
	w := request(ui, "POST", "/login", url.Values{"username": {"admin"}, "password": {"admin"}, "csrf": {csrf}}.Encode(), cookies)
	if w.Code != 303 {
		t.Fatalf("login %d %s", w.Code, w.Body.String())
	}
	session := w.Result().Cookies()
	if w = request(ui, "GET", "/", "", session); w.Code != 303 || w.Header().Get("Location") != "/password" {
		t.Fatalf("bootstrap data access %d %s", w.Code, w.Body.String())
	}
	if w = request(ui, "POST", "/password", "password=new-password-long", session); w.Code != 403 {
		t.Fatalf("CSRF bypass %d", w.Code)
	}
	var sessionID string
	for _, c := range session {
		if c.Name == "session" {
			sessionID = c.Value
		}
	}
	token, err := h.store.Session(context.Background(), hashToken(sessionID), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"abcd!", "abcdef", "abcde ", strings.Repeat("a", 72) + "!"} {
		w = request(ui, "POST", "/password", url.Values{"password": {password}, "csrf": {token}}.Encode(), session)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid password accepted: status %d", w.Code)
		}
	}
	w = request(ui, "POST", "/password", url.Values{"password": {"abcde!"}, "csrf": {token}}.Encode(), session)
	if w.Code != 303 {
		t.Fatalf("password change %d %s", w.Code, w.Body.String())
	}
	if w = request(ui, "GET", "/", "", session); w.Code != 303 {
		t.Fatal("old session survives password change")
	}
	hash, _ := h.store.Setting(context.Background(), "password")
	if strings.Contains(hash, "abcde!") || hash == "admin" {
		t.Fatal("plaintext password")
	}
}
func TestProxyIdentityTrustAndNoFallback(t *testing.T) {
	h := testHub(t, "proxy")
	ui := h.UIHandler()
	for _, remote := range []string{"10.0.0.2:1234", "127.0.0.1:1234"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-authentik-username", "forged")
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("forged identity %d", w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-authentik-username", "user")
	w := httptest.NewRecorder()
	ui.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "node") {
		t.Fatalf("proxy denied or missing configured host %d %s", w.Code, w.Body.String())
	}
	if w = request(ui, "GET", "/login", "", nil); w.Code != 403 {
		t.Fatal("local auth fallback")
	}
}
func TestIngestionAuthenticationAndValidation(t *testing.T) {
	h := testHub(t, "local")
	api := h.AgentHandler()
	s := protocol.Sample{Version: 1, HostID: "node", ID: "one", At: time.Now(), Metrics: []protocol.Metric{{Name: "cpu_percent", Value: 30, Weight: 1}}}
	b, _ := json.Marshal(s)
	for _, x := range []struct {
		token, host string
		body        []byte
		want        int
	}{{"", "node", b, 401}, {strings.Repeat("s", 32), "foreign", b, 401}, {strings.Repeat("s", 32), "node", []byte(`{"version":999}`), 400}, {strings.Repeat("s", 32), "node", b, 204}, {strings.Repeat("s", 32), "node", b, 204}} {
		r := httptest.NewRequest("POST", "/v1/samples", bytes.NewReader(x.body))
		r.Header.Set("X-Host-ID", x.host)
		r.Header.Set("Authorization", "Bearer "+x.token)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != x.want {
			t.Fatalf("ingestion %d want %d %s", w.Code, x.want, w.Body.String())
		}
	}
	p, _ := h.store.History(context.Background(), "node", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), true, 300)
	if len(p) != 1 || p[0].Count != 1 {
		t.Fatalf("dedup %+v", p)
	}
	r := httptest.NewRequest("GET", "/v1/logs/next", nil)
	r.Header.Set("X-authentik-username", "admin")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("proxy credential accepted for agent")
	}
}
func TestLogQueueOwnershipAndBounds(t *testing.T) {
	h := testHub(t, "local")
	req, done, err := h.logs.create("node", "abc", h.config)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = h.logs.create("node", "abc", h.config); err == nil {
		t.Fatal("unbounded per-host queue")
	}
	if h.logs.next("foreign") != nil {
		t.Fatal("foreign agent claimed logs")
	}
	if next := h.logs.next("node"); next == nil || next.ID != req.ID {
		t.Fatal("request missing")
	}
	if h.logs.complete("foreign", protocol.LogResult{ID: req.ID, Text: "secret"}, h.config.LogBytes) == nil {
		t.Fatal("foreign response accepted")
	}
	if err = h.logs.complete("node", protocol.LogResult{ID: req.ID, Text: "<script>evil</script>"}, h.config.LogBytes); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.Text != "<script>evil</script>" {
			t.Fatal("lost result")
		}
	case <-time.After(time.Second):
		t.Fatal("result not delivered")
	}
	if h.logs.complete("node", protocol.LogResult{ID: req.ID}, h.config.LogBytes) == nil {
		t.Fatal("duplicate result accepted")
	}
	h.logs.remove(req.ID)
}
func TestEscapedLogOutput(t *testing.T) {
	h := testHub(t, "proxy")
	w := httptest.NewRecorder()
	h.render(w, "logs", map[string]any{"Text": "<script>alert(1)</script>"})
	b, _ := io.ReadAll(w.Result().Body)
	if strings.Contains(string(b), "<script>alert") || !strings.Contains(string(b), "&lt;script&gt;") {
		t.Fatalf("unsafe output %s", b)
	}
}

func TestLogTimeoutOverflowAndOrigin(t *testing.T) {
	h := testHub(t, "proxy")
	req, _, err := h.logs.create("node", "abc", h.config)
	if err != nil {
		t.Fatal(err)
	}
	h.logs.next("node")
	if h.logs.complete("node", protocol.LogResult{ID: req.ID, Text: strings.Repeat("x", h.config.LogBytes+1)}, h.config.LogBytes) == nil {
		t.Fatal("oversized log result accepted")
	}
	h.logs.mu.Lock()
	h.logs.items[req.ID].request.Expires = time.Now().Add(-time.Second)
	h.logs.mu.Unlock()
	if h.logs.complete("node", protocol.LogResult{ID: req.ID}, h.config.LogBytes) == nil {
		t.Fatal("expired response accepted")
	}
	r := httptest.NewRequest("POST", "/hosts/node/logs/abc", nil)
	r.RemoteAddr = "10.0.0.1:12"
	r.Header.Set("X-authentik-username", "user")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.UIHandler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin mutation")
	}
}

func TestLoginRateLimitedAndBootstrapPersists(t *testing.T) {
	h := testHub(t, "local")
	hash, _ := h.store.Setting(context.Background(), "password")
	if err := h.initLocal(); err != nil {
		t.Fatal(err)
	}
	again, _ := h.store.Setting(context.Background(), "password")
	if hash != again {
		t.Fatal("bootstrap reset on restart")
	}
	ui := h.UIHandler()
	login := request(ui, "GET", "/login", "", nil)
	cookies := login.Result().Cookies()
	var csrf string
	for _, c := range cookies {
		if c.Name == "login_csrf" {
			csrf = c.Value
		}
	}
	var w *httptest.ResponseRecorder
	for i := 0; i < 11; i++ {
		w = request(ui, "POST", "/login", url.Values{"username": {"admin"}, "password": {"wrong"}, "csrf": {csrf}}.Encode(), cookies)
	}
	if w.Code != 429 {
		t.Fatalf("unbounded login attempts: %d", w.Code)
	}
}

func TestUIOverloadIsBounded(t *testing.T) {
	h := testHub(t, "local")
	for i := 0; i < h.config.MaxRequests; i++ {
		h.uiSlots <- struct{}{}
	}
	w := request(h.UIHandler(), "GET", "/login", "", nil)
	if w.Code != 503 {
		t.Fatalf("unbounded UI work: %d", w.Code)
	}
}

func TestLocalBootstrapCannotBePublishedThroughPrivateProxy(t *testing.T) {
	h := testHub(t, "local")
	h.config.PublicURL = "https://monitor.example.com"
	if err := h.initLocal(); err == nil {
		t.Fatal("uninitialized admin/admin permitted on public origin")
	}
	if err := h.store.ChangePassword(context.Background(), "test-hash"); err != nil {
		t.Fatal(err)
	}
	if err := h.initLocal(); err != nil {
		t.Fatal("initialized local auth should permit HTTPS ingress")
	}
}
