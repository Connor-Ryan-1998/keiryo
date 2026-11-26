package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAlertPolicyConfig(t *testing.T) {
	for _, tc := range []struct {
		policy string
		valid  bool
	}{
		{`{}`, true},
		{`{"resources":{"cpu":{"warning":0,"critical":100}}}`, true},
		{`{"resources":{"memory":{"warning":65}}}`, true},
		{`{"containers":{"unhealthy":"critical"}}`, true},
		{`{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot","failure":"critical"}]}`, true},
		{`{"resources":{"cpu":{"warning":90,"critical":80}}}`, false},
		{`{"resources":{"cpu":{"warning":80,"critical":80}}}`, false},
		{`{"resources":{"disk":{"warning":-1}}}`, false},
		{`{"resources":{"memory":{"critical":101}}}`, false},
		{`{"resources":{"gpu":{"warning":10}}}`, false},
		{`{"containers":{"stopped":"healthy"}}`, false},
		{`{"containers":{"starting":"typo"}}`, false},
		{`{"container_overrides":[{"host":"node","name":"init","lifecycle":"typo"}]}`, false},
		{`{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot","failure":"healthy"}]}`, false},
		{`{"container_overrides":[{"name":"init","lifecycle":"oneshot"}]}`, false},
		{`{"container_overrides":[{"host":"node","name":"","lifecycle":"oneshot"}]}`, false},
		{`{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot"},{"host":"node","name":"init","lifecycle":"oneshot"}]}`, false},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.json")
			if err := os.WriteFile(path, []byte(`{"alert_policy":`+tc.policy+`}`), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadHub(path); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestExpectedContainersConfig(t *testing.T) {
	for _, tc := range []struct {
		data  string
		valid bool
	}{
		{`{"agents":{"node":"t"},"expected_containers":[{"host":"node","name":"traefik"}]}`, true},
		{`{"agents":{"node":"t"},"expected_containers":[{"host":"node","name":"traefik","severity":"warning"}]}`, true},
		{`{"agents":{"node":"t"},"expected_containers":[{"host":"other","name":"traefik"}]}`, false},
		{`{"agents":{"node":"t"},"expected_containers":[{"host":"node","name":""}]}`, false},
		{`{"agents":{"node":"t"},"expected_containers":[{"host":"node","name":"a/b"}]}`, false},
		{`{"agents":{"node":"t"},"expected_containers":[{"host":"node","name":"x","severity":"info"}]}`, false},
		{`{"agents":{"node":"t"},"expected_containers":[{"host":"node","name":"x"},{"host":"node","name":"x"}]}`, false},
	} {
		path := filepath.Join(t.TempDir(), "hub.json")
		os.WriteFile(path, []byte(tc.data), 0600)
		c, err := LoadHub(path)
		if (err == nil) != tc.valid {
			t.Fatalf("%s: valid=%v err=%v", tc.data, tc.valid, err)
		}
		if err == nil && c.ExpectedContainers[0].Severity == "" {
			t.Fatal("expected container severity default not applied")
		}
	}
}

func TestRestartLoopPolicyConfig(t *testing.T) {
	for _, tc := range []struct {
		policy string
		valid  bool
	}{
		{`{"containers":{"restart_loop":{"restarts":5}}}`, true},
		{`{"containers":{"restart_loop":{"restarts":0}}}`, true},
		{`{"containers":{"restart_loop":{"restarts":3,"window_seconds":1800,"severity":"critical"}}}`, true},
		{`{"container_overrides":[{"host":"node","name":"worker","restart_loop":{"restarts":0}}]}`, true},
		{`{"container_overrides":[{"host":"node","name":"worker","restart_loop":{"restarts":10,"window_seconds":3600}}]}`, true},
		{`{"container_overrides":[{"host":"node","name":"init","lifecycle":"oneshot","restart_loop":{"restarts":1}}]}`, true},
		{`{"container_overrides":[{"host":"node","name":"worker"}]}`, false},
		{`{"containers":{"restart_loop":{"restarts":-1}}}`, false},
		{`{"containers":{"restart_loop":{"restarts":3,"window_seconds":30}}}`, false},
		{`{"containers":{"restart_loop":{"restarts":3,"window_seconds":90000}}}`, false},
		{`{"containers":{"restart_loop":{"restarts":3,"severity":"info"}}}`, false},
		{`{"container_overrides":[{"host":"node","name":"worker","restart_loop":{"restarts":3,"severity":"info"}}]}`, false},
		{`{"container_overrides":[{"host":"node","name":"worker","restart_loop":{"restarts":3,"window_seconds":10}}]}`, false},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.json")
			os.WriteFile(path, []byte(`{"alert_policy":`+tc.policy+`}`), 0600)
			if _, err := LoadHub(path); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "hub.json")
	os.WriteFile(path, []byte(`{}`), 0600)
	c, _ := LoadHub(path)
	if got := c.AlertPolicy.Containers.RestartLoop; got.Restarts != 3 || got.WindowSeconds != 600 || got.Severity != "warning" {
		t.Fatalf("restart loop defaults %+v", got)
	}
	// The window must fit inside raw retention, where container history lives.
	os.WriteFile(path, []byte(`{"raw_hours":1,"summary_days":1,"alert_policy":{"containers":{"restart_loop":{"window_seconds":7200}}}}`), 0600)
	if _, err := LoadHub(path); err == nil {
		t.Fatal("restart window longer than raw retention accepted")
	}
}

func TestDiskForecastConfig(t *testing.T) {
	for _, tc := range []struct {
		policy string
		valid  bool
	}{
		{`{"resources":{"disk":{"warning":70,"forecast":{"days":14}}}}`, true},
		{`{"resources":{"disk":{"forecast":{"days":0}}}}`, true},
		{`{"resources":{"disk":{"forecast":{"days":7,"window_hours":168,"severity":"critical"}}}}`, true},
		{`{"resources":{"disk":{"forecast":{"days":-1}}}}`, false},
		{`{"resources":{"disk":{"forecast":{"window_hours":12}}}}`, false},
		{`{"resources":{"disk":{"forecast":{"severity":"info"}}}}`, false},
		{`{"resources":{"disk":{"forecast":{"window_hours":2000}}}}`, false},
		{`{"resources":{"cpu":{"forecast":{"days":7}}}}`, false},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.json")
			os.WriteFile(path, []byte(`{"alert_policy":`+tc.policy+`}`), 0600)
			if _, err := LoadHub(path); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "hub.json")
	os.WriteFile(path, []byte(`{"alert_policy":{"resources":{"disk":{"warning":70}}}}`), 0600)
	c, _ := LoadHub(path)
	if d := c.AlertPolicy.Resources.Disk; d.Warning != 70 || d.Critical != 90 || d.Forecast.Days != 7 || d.Forecast.WindowHours != 72 || d.Forecast.Severity != "warning" {
		t.Fatalf("disk policy defaults %+v", d)
	}
}
