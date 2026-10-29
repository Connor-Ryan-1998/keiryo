package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortableConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.json")
	if err := os.WriteFile(path, []byte(`{"agents":{"node":"node.token"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadHub(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RawHours != 24 || c.SummaryDays != 30 || c.BucketSeconds != 300 || c.Agents["node"] != filepath.Join(dir, "node.token") {
		t.Fatalf("incorrect defaults/paths: %+v", c)
	}
	if err := os.WriteFile(path, []byte(`{"secret_typo":"do-not-echo-me"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadHub(path)
	if err == nil || strings.Contains(err.Error(), "do-not-echo-me") {
		t.Fatalf("unsafe unknown config: %v", err)
	}
}

func TestRejectInvalidConfig(t *testing.T) {
	for _, data := range []string{`null`, `{"raw_hours":0}`, `{"auth_mode":"fallback"}`, `{"stale_seconds":400,"offline_seconds":300}`, `{"raw_hours":900,"summary_days":1}`, `{} {}`, `{"trusted_proxies":["not-a-cidr"]}`} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.json")
			os.WriteFile(path, []byte(data), 0600)
			if _, err := LoadHub(path); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestAgentDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	os.WriteFile(path, []byte(`{"host_id":"node","hub_url":"http://127.0.0.1:9081","credential_file":"node.token"}`), 0600)
	c, err := LoadAgent(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.IntervalSeconds != 60 || c.BufferSamples != 60 || c.PollSeconds != 5 {
		t.Fatalf("wrong config: %+v", c)
	}
	for _, data := range []string{`{}`, `{"host_id":"../bad","hub_url":"http://localhost:9081","credential_file":"x"}`, `{"host_id":"n","hub_url":"file:///etc/passwd","credential_file":"x"}`} {
		os.WriteFile(path, []byte(data), 0600)
		if _, err := LoadAgent(path); err == nil {
			t.Fatal("accepted invalid agent")
		}
	}
}

func TestAgentOTLPEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	for _, tc := range []struct {
		endpoint string
		valid    bool
	}{{"", true}, {"https://collector.example/v1/metrics", true}, {"file:///etc/passwd", false}, {"http://user:secret@collector/v1/metrics", false}, {"https://collector/v1/metrics#secret", false}, {"https://collector/v1/metrics?token=secret", false}} {
		data := `{"host_id":"node","hub_url":"http://127.0.0.1:9081/","credential_file":"node.token","otlp_metrics_url":"` + tc.endpoint + `"}`
		os.WriteFile(path, []byte(data), 0600)
		c, err := LoadAgent(path)
		if (err == nil) != tc.valid {
			t.Fatal(tc, err)
		}
		if tc.valid {
			want := tc.endpoint
			if want == "" {
				want = "http://127.0.0.1:9081/v1/metrics"
			}
			if c.MetricsURL() != want {
				t.Fatal(c.MetricsURL())
			}
		}
	}
}

func TestDiskHealthMountpointsConfig(t *testing.T) {
	for _, tc := range []struct {
		data  string
		valid bool
	}{
		{`{"disk_health_mountpoints":["/","/data"]}`, true},
		{`{"disk_health_mountpoints":[]}`, true},
		{`{"disk_health_mountpoints":[""]}`, false},
		{`{"disk_health_mountpoints":["data"]}`, false},
		{`{"disk_health_mountpoints":["/data/../"]}`, false},
	} {
		t.Run(tc.data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.json")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadHub(path); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}
