package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPChecksConfig(t *testing.T) {
	for _, tc := range []struct {
		checks string
		valid  bool
	}{
		{`[{"name":"traefik","url":"https://example.com/ping"}]`, true},
		{`[{"name":"immich","url":"http://immich:2283/api/server/ping","expect_status":[200],"interval_seconds":30,"timeout_seconds":5}]`, true},
		{`[{"name":"a","url":"https://example.com","tls_warn_days":0,"tls_critical_days":0,"severity":"warning","follow_redirects":true}]`, true},
		{`[{"name":"bad name","url":"https://example.com"}]`, false},
		{`[{"name":"a","url":"ftp://example.com"}]`, false},
		{`[{"name":"a","url":"https://user:pw@example.com"}]`, false},
		{`[{"name":"a","url":"https://example.com#x"}]`, false},
		{`[{"name":"a","url":"https://"}]`, false},
		{`[{"name":"a","url":"https://example.com","interval_seconds":5}]`, false},
		{`[{"name":"a","url":"https://example.com","interval_seconds":10,"timeout_seconds":10}]`, false},
		{`[{"name":"a","url":"https://example.com","expect_status":[99]}]`, false},
		{`[{"name":"a","url":"https://example.com","tls_warn_days":3,"tls_critical_days":7}]`, false},
		{`[{"name":"a","url":"https://example.com","severity":"info"}]`, false},
		{`[{"name":"a","url":"https://example.com","typo":1}]`, false},
		{`[{"name":"a","url":"https://example.com"},{"name":"a","url":"https://example.org"}]`, false},
	} {
		t.Run(tc.checks, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.json")
			os.WriteFile(path, []byte(`{"http_checks":`+tc.checks+`}`), 0600)
			if _, err := LoadHub(path); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "hub.json")
	os.WriteFile(path, []byte(`{"http_checks":[{"name":"a","url":"https://example.com"},{"name":"b","url":"https://example.org","tls_warn_days":0,"tls_critical_days":0}]}`), 0600)
	c, err := LoadHub(path)
	if err != nil {
		t.Fatal(err)
	}
	if a := c.HTTPChecks[0]; a.IntervalSeconds != 60 || a.TimeoutSeconds != 10 || a.TLSWarnDays != 14 || a.TLSCriticalDays != 3 || a.Severity != "critical" || a.FollowRedirects {
		t.Fatalf("check defaults %+v", a)
	}
	if b := c.HTTPChecks[1]; b.TLSWarnDays != 0 || b.TLSCriticalDays != 0 {
		t.Fatalf("explicit zero TLS days not kept %+v", b)
	}
}
