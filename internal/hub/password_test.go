package hub

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"testing"
	"time"
)

// localLogin signs in through the real form and returns the session cookies and CSRF token.
func localLogin(t *testing.T, h *Hub, password string) ([]*http.Cookie, string) {
	t.Helper()
	ui := h.UIHandler()
	page := request(ui, "GET", "/login", "", nil)
	cookies := page.Result().Cookies()
	var csrf string
	for _, c := range cookies {
		if c.Name == "login_csrf" {
			csrf = c.Value
		}
	}
	w := request(ui, "POST", "/login", url.Values{"username": {"admin"}, "password": {password}, "csrf": {csrf}}.Encode(), cookies)
	if w.Code != 303 {
		t.Fatalf("login with %q: %d %s", password, w.Code, w.Body.String())
	}
	session := w.Result().Cookies()
	for _, c := range session {
		if c.Name == "session" {
			token, err := h.store.Session(context.Background(), hashToken(c.Value), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			return session, token
		}
	}
	t.Fatal("no session cookie")
	return nil, ""
}

func TestPasswordChangeRequiresCurrentPasswordAfterBootstrap(t *testing.T) {
	h := testHub(t, "local")
	ui := h.UIHandler()
	// Bootstrap: the admin/admin login already proved the default password.
	session, csrf := localLogin(t, h, "admin")
	if w := request(ui, "POST", "/password", url.Values{"password": {"first-pass!"}, "csrf": {csrf}}.Encode(), session); w.Code != 303 {
		t.Fatalf("bootstrap password change %d %s", w.Code, w.Body.String())
	}
	if page := request(ui, "GET", "/password", "", session); page.Code != 303 {
		t.Fatal("bootstrap session survived password change")
	}
	session, csrf = localLogin(t, h, "first-pass!")
	for _, current := range []string{"", "wrong-pass!"} {
		w := request(ui, "POST", "/password", url.Values{"current": {current}, "password": {"second-pass!"}, "csrf": {csrf}}.Encode(), session)
		if w.Code != http.StatusForbidden {
			t.Fatalf("current password %q accepted: %d", current, w.Code)
		}
	}
	if w := request(ui, "POST", "/password", url.Values{"current": {"first-pass!"}, "password": {"second-pass!"}, "csrf": {csrf}}.Encode(), session); w.Code != 303 {
		t.Fatalf("password change with current password %d %s", w.Code, w.Body.String())
	}
	localLogin(t, h, "second-pass!")
}

func TestPasswordPageAsksForCurrentPasswordOnlyAfterBootstrap(t *testing.T) {
	h := testHub(t, "local")
	ui := h.UIHandler()
	session, csrf := localLogin(t, h, "admin")
	if body := request(ui, "GET", "/password", "", session).Body.String(); containsCurrent(body) {
		t.Fatal("bootstrap form asks for the default password again")
	}
	request(ui, "POST", "/password", url.Values{"password": {"first-pass!"}, "csrf": {csrf}}.Encode(), session)
	session, _ = localLogin(t, h, "first-pass!")
	if body := request(ui, "GET", "/password", "", session).Body.String(); !containsCurrent(body) {
		t.Fatal("password form missing current password field")
	}
}

func containsCurrent(body string) bool {
	return regexpCurrent.MatchString(body)
}

var regexpCurrent = regexp.MustCompile(`name="current"`)
