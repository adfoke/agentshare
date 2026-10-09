// Command agentshare runs the file exchange service.
//
// Everything it needs is on the command line or in the environment; everything
// it owns is inside one data directory. There is no configuration file, and no
// state anywhere else on the machine.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/adfoke/agentshare/internal/api"
	"github.com/adfoke/agentshare/internal/blob"
	"github.com/adfoke/agentshare/internal/config"
	"github.com/adfoke/agentshare/internal/id"
	"github.com/adfoke/agentshare/internal/store"
	"github.com/adfoke/agentshare/internal/version"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, config.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "agentshare: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}
	if cfg.ShowVersion {
		fmt.Println(version.Full())
		return nil
	}

	log := newLogger(cfg)

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	absData, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return err
	}

	st, err := store.Open(filepath.Join(absData, "agentshare.db"))
	if err != nil {
		return fmt.Errorf("open metadata: %w", err)
	}
	defer st.Close()

	blobs, err := blob.Open(absData)
	if err != nil {
		return fmt.Errorf("open blob store: %w", err)
	}

	adminToken, generated, err := resolveAdminToken(cfg, log)
	if err != nil {
		return err
	}
	if generated {
		log.Warn("generated a root token; it is saved in the data directory",
			"path", filepath.Join(absData, "admin.token"),
			"token", adminToken,
			"next", "put it in AGENTSHARE_ADMIN_TOKEN for the service, or mint scoped tokens with POST /v1/tokens")
	}
	if len(adminToken) < 16 {
		log.Warn("the configured root token is short; use at least 32 random characters",
			"length", len(adminToken))
	}

	srv, err := api.New(cfg, st, blobs, log, adminToken)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
		// No ReadTimeout or WriteTimeout on purpose: a large upload from a slow
		// link is a legitimate request, not a stuck one. The header read is
		// bounded so a half-open socket cannot hold a slot forever, and the
		// body is bounded separately by --max-upload.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.JanitorEvery > 0 {
		go runJanitor(ctx, st, blobs, cfg.JanitorEvery, log)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("agentshare is listening",
			"version", version.Full(),
			"listen", cfg.Listen,
			"data_dir", absData,
			"base_url", orDefault(cfg.BaseURL, "(derived from each request)"),
			"max_upload_bytes", cfg.MaxUpload,
			"default_ttl", cfg.DefaultTTL.String(),
			"janitor_interval", cfg.JanitorEvery.String(),
			"endpoints", "GET /  GET /llms.txt  GET /openapi.json")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	case <-ctx.Done():
	}

	log.Info("shutting down; waiting for in-flight transfers")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown did not finish cleanly", "err", err.Error())
	}
	return nil
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// resolveAdminToken finds the root credential: from the flag or environment
// first, then from the data directory, and finally by generating one. The
// generated token is written 0600 so a restart reuses it rather than locking
// everyone out.
func resolveAdminToken(cfg *config.Config, log *slog.Logger) (token string, generated bool, err error) {
	if t := strings.TrimSpace(cfg.AdminToken); t != "" {
		return t, false, nil
	}
	path := filepath.Join(cfg.DataDir, "admin.token")
	if b, readErr := os.ReadFile(path); readErr == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, false, nil
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", false, fmt.Errorf("read %s: %w", path, readErr)
	}

	token = id.Secret()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("write %s: %w", path, err)
	}
	log.Debug("wrote the new root token", "path", path)
	return token, true, nil
}

// runJanitor deletes what has expired and reclaims the disk behind it.
//
// The blob sweep is a full pass over the store rather than a reference count:
// it also collects bytes orphaned by a crash, and it runs rarely enough that
// the cost is irrelevant next to the certainty it buys.
func runJanitor(ctx context.Context, st *store.Store, blobs *blob.Store, every time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	sweep := func(first bool) {
		now := time.Now()
		res, err := st.SweepExpired(ctx, now)
		if err != nil {
			log.Error("janitor: sweep failed", "err", err.Error())
			return
		}
		if first || res.Files > 0 {
			refs, err := st.AllDigests(ctx)
			if err != nil {
				log.Error("janitor: cannot read referenced digests", "err", err.Error())
			} else {
				removed, freed, err := blobs.GC(func(sum string) bool {
					_, ok := refs[sum]
					return ok
				})
				if err != nil {
					log.Error("janitor: blob collection failed", "err", err.Error())
				} else if removed > 0 {
					log.Info("janitor: reclaimed blobs", "removed", removed, "bytes_freed", freed)
				}
			}
		}
		if n, err := blobs.CleanTemp(now.Add(-time.Hour)); err != nil {
			log.Error("janitor: temp cleanup failed", "err", err.Error())
		} else if n > 0 {
			log.Info("janitor: removed abandoned uploads", "count", n)
		}
		if res.Files > 0 || res.Shares > 0 || res.Tokens > 0 {
			log.Info("janitor: swept expired records",
				"files", res.Files, "shares", res.Shares, "tokens", res.Tokens)
		}
	}

	sweep(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep(false)
		}
	}
}
