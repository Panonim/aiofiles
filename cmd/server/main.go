// Command server runs the aiofiles HTTP API and job workers.
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
	"syscall"
	"time"

	"aiofiles/internal/api"
	"aiofiles/internal/auth"
	"aiofiles/internal/config"
	"aiofiles/internal/db"
	"aiofiles/internal/jobs"
	"aiofiles/internal/retention"
	"aiofiles/internal/runner"
)

func main() {
	hashPassword := flag.String("hash-password", "", "print an argon2id hash for the given password and exit")
	flag.Parse()

	if *hashPassword != "" {
		h, err := auth.HashPassword(*hashPassword)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hash password:", err)
			os.Exit(1)
		}
		fmt.Println(h)
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	// SIGTERM from s6/Docker cancels ctx, unwinding the workers, the sweeper,
	// the session reaper and any running subprocess.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sqlDB, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	store := jobs.NewStore(sqlDB)
	bus := jobs.NewBus()

	// Jobs left "running" belong to a previous process whose children died with
	// it: there is nothing to resume, so they are failed. Jobs left "queued"
	// never started and are re-enqueued below.
	if n, err := store.FailInterrupted(ctx); err != nil {
		log.Warn("recover interrupted jobs", "err", err)
	} else if n > 0 {
		log.Info("marked interrupted jobs as failed", "count", n)
	}

	ytdlp := runner.NewYtDlp(cfg, log)
	ffmpeg := runner.NewFFmpeg(cfg, log)
	image := runner.NewImage(cfg, log)

	queue := jobs.NewQueue(store, bus, log, cfg.QueueDepth)
	queue.SetTimeout(cfg.JobTimeout())
	queue.Register(jobs.TypeDownload, ytdlp)
	queue.Register(jobs.TypeConvert, ffmpeg)
	queue.Register(jobs.TypeCompress, ffmpeg)
	queue.Register(jobs.TypeImage, image)
	queue.Register(jobs.TypeEdit, ffmpeg)
	if n, err := queue.Resume(ctx); err != nil {
		log.Warn("resume queued jobs", "err", err)
	} else if n > 0 {
		log.Info("re-enqueued jobs interrupted while queued", "count", n)
	}
	queue.Start(ctx, cfg.MaxConcurrentJobs)

	authManager := auth.NewManager(cfg)
	// Persist sessions in the job database so a restart does not sign everyone out.
	if err := authManager.Persist(auth.NewSQLSessionStore(sqlDB)); err != nil {
		log.Warn("restore sessions", "err", err)
	}
	authManager.StartReaper(ctx)

	retention.NewSweeper(store, bus, log, retention.DefaultInterval).
		WithUploadGC(cfg.UploadDir, retention.DefaultUploadGrace).
		Start(ctx)

	handler := api.New(api.Deps{
		Cfg:   cfg,
		Store: store,
		Queue: queue,
		Bus:   bus,
		Log:   log,
		Auth:  authManager,
		Probe: func(ctx context.Context, rawURL string) (any, error) {
			info, err := ytdlp.Probe(ctx, rawURL)
			if err != nil {
				return nil, err // never hand back a typed nil pointer as `any`
			}
			return info, nil
		},
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: SSE streams and multi-gigabyte uploads are both
		// long-lived by design, and a server-wide deadline would sever them.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "auth", cfg.AuthEnabled(),
			"workers", cfg.MaxConcurrentJobs)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	queue.Stop()
	return nil
}
