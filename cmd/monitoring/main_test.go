package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRustDispatchChild(t *testing.T) {
	if os.Getenv("MONITORING_DISPATCH_CHILD") != "1" {
		return
	}
	if err := dispatchAgent(os.Getenv("MONITORING_DISPATCH_BINARY"), []string{"agent", "-config", "relative.json"}); err != nil {
		t.Fatal(err)
	}
}

func TestRustDispatchPreservesArgumentsEnvironmentAndExit(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "monitoring-agent")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" \"$DISPATCH_VALUE\"\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRustDispatchChild$")
	cmd.Env = append(os.Environ(), "MONITORING_DISPATCH_CHILD=1", "MONITORING_DISPATCH_BINARY="+binary, "DISPATCH_VALUE=preserved")
	output, err := cmd.CombinedOutput()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 7 {
		t.Fatalf("exit: %v, %s", err, output)
	}
	if !strings.Contains(string(output), "agent\n-config\nrelative.json\npreserved\n") {
		t.Fatalf("arguments/env lost: %s", output)
	}
	if dispatchAgent(filepath.Join(dir, "missing"), nil) == nil {
		t.Fatal("missing agent silently accepted")
	}
}

func TestRejectMissingModeAndConfig(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"hub"}, {"agent", "-config", "missing.json"}} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestFlagFormsChild(t *testing.T) {
	if os.Getenv("MONITORING_FLAG_FORM") == "" {
		return
	}
	if err := run(context.Background(), strings.Split(os.Getenv("MONITORING_FLAG_FORM"), "\n")); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRetainsPreviouslyAcceptedConfigFlagForms(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(dir, "monitoring.test")
	if err = os.WriteFile(copy, data, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "monitoring-agent"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"agent", "-config=relative.json"}, {"agent", "--config", "relative.json"}, {"agent", "--config=relative.json"}} {
		cmd := exec.Command(copy, "-test.run=^TestFlagFormsChild$")
		cmd.Env = append(os.Environ(), "MONITORING_FLAG_FORM="+strings.Join(args, "\n"))
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, output)
		}
		if string(output) != "agent\n-config\nrelative.json\n" {
			t.Fatalf("flag form not normalized: %v: %s", args, output)
		}
	}
}
