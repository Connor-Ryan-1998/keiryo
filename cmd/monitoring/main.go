package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/hub"
	"houselab/monitoring/internal/store"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("monitoring stopped", "error", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "hub" && args[0] != "agent") {
		return errors.New("usage: monitoring hub|agent -config path.json")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := flags.String("config", "", "JSON configuration path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("a configuration file is required")
	}
	if args[0] == "agent" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		return dispatchAgent(filepath.Join(filepath.Dir(executable), "monitoring-agent"), []string{"agent", "-config", *path})
	}
	c, err := config.LoadHub(*path)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(c.Database), 0700); err != nil {
		return err
	}
	s, err := store.Open(c.Database)
	if err != nil {
		return err
	}
	defer s.Close()
	h, err := hub.New(c, s)
	if err != nil {
		return err
	}
	ui, err := net.Listen("tcp", c.UIListen)
	if err != nil {
		return err
	}
	defer ui.Close()
	api, err := net.Listen("tcp", c.AgentListen)
	if err != nil {
		return err
	}
	defer api.Close()
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	go h.Maintenance(child)
	servers := []*http.Server{{Handler: h.UIHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: time.Duration(c.LogTimeoutSeconds+15) * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}, {Handler: h.AgentHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}}
	servers[0].BaseContext = func(net.Listener) context.Context { return child }
	failures := make(chan error, 2)
	go func() { failures <- servers[0].Serve(ui) }()
	go func() { failures <- servers[1].Serve(api) }()
	slog.Info("monitoring hub listening", "ui", c.UIListen, "agents", c.AgentListen)
	select {
	case <-ctx.Done():
	case err = <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			err = fmt.Errorf("HTTP listener failed: %w", err)
		}
	}
	cancel()
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	for _, server := range servers {
		if shutdownErr := server.Shutdown(shutdown); shutdownErr != nil {
			server.Close()
			if err == nil {
				err = shutdownErr
			}
		}
	}
	return err
}
