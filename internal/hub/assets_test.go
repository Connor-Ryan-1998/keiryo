package hub

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestVersionedStaticAssetsAreCacheable(t *testing.T) {
	h := testHub(t, "proxy")
	page := serveUI(t, h, "/")
	src := regexp.MustCompile(`src="(/static/echarts\.min\.js\?v=[0-9a-f]+)"`).FindStringSubmatch(page)
	if src == nil {
		t.Fatal("page does not reference a versioned echarts asset")
	}
	for _, want := range []string{`href="/static/style.css?v=`, `src="/static/htmx.min.js?v=`, `src="/static/dashboard.js?v=`, `href="/static/keiryo.svg?v=`} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing versioned asset %s", want)
		}
	}
	get := func(target string) string {
		w := httptest.NewRecorder()
		h.UIHandler().ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", target, w.Code)
		}
		return w.Header().Get("Cache-Control")
	}
	if got := get(src[1]); !strings.Contains(got, "immutable") || strings.Contains(got, "no-store") {
		t.Fatalf("current versioned asset Cache-Control %q", got)
	}
	// Unversioned or outdated URLs must not pin content that a later release changes.
	for _, target := range []string{"/static/echarts.min.js", "/static/echarts.min.js?v=0000"} {
		if got := get(target); strings.Contains(got, "immutable") {
			t.Fatalf("%s cached immutably: %q", target, got)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-authentik-username", "test")
	h.UIHandler().ServeHTTP(w, r)
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("dashboard page Cache-Control %q", got)
	}
}
