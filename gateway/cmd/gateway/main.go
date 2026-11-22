// Command gateway is the Argus Go gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/argus-video/argus/gateway/internal/api"
	"github.com/argus-video/argus/gateway/internal/auth"
	"github.com/argus-video/argus/gateway/internal/config"
	"github.com/argus-video/argus/gateway/internal/media"
	"github.com/argus-video/argus/gateway/internal/session"
	"github.com/argus-video/argus/gateway/internal/streams"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

func run() error {
	cfgPath := flag.String("config", "gateway.yaml", "path to gateway.yaml")
	listen := flag.String("listen", "", "override listen address")
	logLevel := flag.String("log-level", "", "override log level (debug|info|warn|error)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	slog.SetDefault(log)

	m, err := media.New(media.StoreConfig{
		RetentionNs:       int64(cfg.Store.RetentionSec) * 1e9,
		ThinAfterNs:       int64(cfg.Store.ThinAfterSec) * 1e9,
		ThinnedIntervalNs: int64(cfg.Store.ThinnedIntervalMs) * 1e6,
		MaxBytes:          cfg.Store.MaxBytes,
	})
	if err != nil {
		return fmt.Errorf("media: %w", err)
	}
	defer m.Close()
	log.Info("media backend", "mediacore", media.Real)

	var specs []auth.TokenSpec
	for _, t := range cfg.Tokens {
		specs = append(specs, auth.TokenSpec{Name: t.Name, Token: t.Token, MaxStreams: t.MaxStreams, MaxSessions: t.MaxSessions})
	}
	a := auth.New(specs, cfg.InternalToken)
	reg := streams.NewRegistry(m, streams.URIPolicy{MediaRoot: cfg.MediaRoot, AllowedSchemes: cfg.AllowedSchemes, AllowedHosts: cfg.AllowedHosts}, log)
	sessions := session.NewManager()
	srv := api.New(api.Deps{Auth: a, Streams: reg, Sessions: sessions, AgentURL: cfg.AgentURL, InternalToken: cfg.InternalToken,
		ExportRoot: filepath.Join(cfg.MediaRoot, "exports"), OriginPatterns: cfg.AllowedOrigins, Log: log})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reg.Start(ctx)

	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("gateway listening", "addr", cfg.Listen)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sessions.CloseAll()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	reg.Close()
	return nil
}
