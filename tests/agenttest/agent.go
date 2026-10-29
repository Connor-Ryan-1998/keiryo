// Package agenttest starts the real Rust agent for integration tests.
package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"houselab/monitoring/internal/config"
)

func Start(t *testing.T, ctx context.Context, c config.Agent) <-chan error {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["queue_database"] = filepath.Join(dir, "agent.redb")
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	binary := os.Getenv("MONITORING_AGENT_BINARY")
	if binary == "" {
		binary, err = filepath.Abs("../../bin/monitoring-agent")
		if err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(ctx, binary, "agent", "-config", path)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	logs, err := os.Create(filepath.Join(dir, "agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err = cmd.Start(); err != nil {
		logs.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		err := cmd.Wait()
		// CommandContext reports cancellation even when SIGTERM exits cleanly.
		if errors.Is(err, context.Canceled) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			err = nil
		}
		done <- err
		close(finished)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-finished
		logs.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logs.Name())
			t.Logf("Rust agent: %s", data)
		}
	})
	return done
}
