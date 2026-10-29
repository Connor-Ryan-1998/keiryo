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
