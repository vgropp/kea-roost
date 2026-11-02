// Command kea-roost is a small web interface for a Kea DHCPv4 server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vgropp/kea-roost/internal/auth"
	"github.com/vgropp/kea-roost/internal/config"
	"github.com/vgropp/kea-roost/internal/kea"
	"github.com/vgropp/kea-roost/internal/manager"
	"github.com/vgropp/kea-roost/internal/web"
)

// version is set when a release is built.
var version = "dev"

func main() {
	run := run
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			run = healthcheck
		case "version":
			fmt.Println(version)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kea-roost:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	options := kea.Options{
		URL:        cfg.KeaURL,
		User:       cfg.KeaUser,
		Password:   cfg.KeaPassword,
		CAFile:     cfg.KeaCAFile,
		ClientCert: cfg.KeaClientCert,
		ClientKey:  cfg.KeaClientKey,
	}
	client, err := kea.NewClient(options)
	if err != nil {
		return err
	}
	store, err := manager.NewStore(cfg.DataDir)
	if err != nil {
		return err
	}
	mgr := manager.New(client, store, cfg.ServerName)
	mgr.SetVersion(version)
	if cfg.HostsDir != "" {
		mgr.SetHostsDir(cfg.HostsDir, cfg.HostsDomains)
	}
	for _, p := range cfg.Peers {
		options.URL = p.URL
		peer, err := kea.NewClient(options)
		if err != nil {
			return err
		}
		mgr.AddPeer(p.Name, peer)
	}
	if cfg.Kea6URL != "" {
		options.URL = cfg.Kea6URL
		client6, err := kea.NewClient(options)
		if err != nil {
			return err
		}
		mgr.SetDHCP6(client6)
		for _, p := range cfg.Peers6 {
			options.URL = p.URL
			peer, err := kea.NewClient(options)
			if err != nil {
				return err
			}
			mgr.AddPeer6(p.Name, peer)
		}
	}
	if cfg.ARPMonitorURL != "" {
		mgr.SetARPMonitor(cfg.ARPMonitorURL)
	}
	if cfg.APDashboardURL != "" {
		mgr.SetAPDashboard(cfg.APDashboardURL)
	}
	if err := mgr.LoadSettings(); err != nil {
		return err
	}
	users, err := auth.LoadUsers(cfg.DataDir)
	if err != nil {
		return err
	}
	setupCode := ""
	if users.Empty() {
		if setupCode, err = auth.RandomToken(); err != nil {
			return err
		}
		// The code goes to the log only: whoever can read it has access to the server.
		slog.Warn("no account yet: open the interface and create the first admin with this setup code", "code", setupCode)
	}
	for _, name := range config.Obsolete(os.Getenv) {
		slog.Warn("setting is no longer used, remove it", "name", name)
	}
	handler := web.New(mgr, web.Auth{Users: users, SetupCode: setupCode, TrustedProxies: cfg.TrustedProxies, MetricsUser: cfg.MetricsUser, MetricsPassword: cfg.MetricsPass, MetricsOff: cfg.MetricsOff}, cfg.BasePath)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go mgr.WatchHostsFile(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			slog.Error("shutdown", "error", err)
		}
	}()

	slog.Info("listening", "version", version, "address", cfg.Listen, "kea", cfg.KeaURL)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-ctx.Done()
	return nil
}

// healthcheck probes the local server. The container image has no shell or curl,
// so the binary doubles as its own probe.
func healthcheck() error {
	listen := os.Getenv("MANAGER_LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}
