package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"
	"go.uber.org/zap"

	"github.com/aigri/whatsapp-bot/internal/cache"
	"github.com/aigri/whatsapp-bot/internal/client"
	"github.com/aigri/whatsapp-bot/internal/config"
	"github.com/aigri/whatsapp-bot/internal/handler"
	"github.com/aigri/whatsapp-bot/internal/model"
	"github.com/aigri/whatsapp-bot/internal/repository"
	"github.com/aigri/whatsapp-bot/internal/service"
	"github.com/aigri/whatsapp-bot/internal/worker"
)

func main() {
	// ---- Logger ------------------------------------------------------------
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialise logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck

	// ---- Config ------------------------------------------------------------
	// .env is optional (environment variables take precedence)
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Warn("godotenv.Load failed (continuing)", zap.Error(err))
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("config.Load failed", zap.Error(err))
	}
	log.Info("configuration loaded",
		zap.String("listen", ":"+cfg.Port),
		zap.Int("workers", cfg.WorkerCount),
		zap.Int("queue_size", cfg.JobQueueSize),
	)

	// ---- Root context with graceful shutdown -------------------------------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- Infrastructure ----------------------------------------------------
	db, err := repository.New(cfg.DSN(), cfg.DBMaxOpen, cfg.DBMaxIdle, log)
	if err != nil {
		log.Fatal("database init failed", zap.Error(err))
	}
	defer db.Close()

	redisClient, err := cache.New(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, log)
	if err != nil {
		log.Fatal("redis init failed", zap.Error(err))
	}
	defer redisClient.Close()

	dedupeWindow := time.Duration(cfg.DedupeWindowMin) * time.Minute

	// ---- External clients --------------------------------------------------
	wahaClient := client.NewWAHAClient(cfg.WAHABaseURL, cfg.WAHAAPIKey, cfg.WAHASession, log)
	ragClient := client.NewRAGClient(cfg.RAGBaseURL, cfg.RAGAPIKey, log)

	// ---- Services ----------------------------------------------------------
	aiHelper := service.NewAIHelper(cfg.OpenAIAPIKey, cfg.OpenAIModel, log)
	routingSvc := service.NewRoutingService(
		db,
		redisClient,
		wahaClient,
		ragClient,
		aiHelper,
		dedupeWindow,
		cfg.WAHAAutoRestart,
		log,
	)

	// ---- Worker pool -------------------------------------------------------
	jobs := make(chan model.MessageJob, cfg.JobQueueSize)
	pool := worker.New(jobs, routingSvc, cfg.WorkerCount, log)
	var wg sync.WaitGroup
	pool.Start(ctx, &wg)

	// ---- HTTP server -------------------------------------------------------
	webhookHandler := handler.NewWebhookHandler(
		jobs,
		redisClient,
		cfg.BotLinkedDeviceID,
		dedupeWindow,
		log,
	)

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(10 * time.Second)) // webhook must return fast

	r.Get("/health", healthHandler)
	r.Post("/webhook", webhookHandler.Handle)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start HTTP server in a background goroutine
	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening", zap.String("addr", ":"+cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// ---- Wait for shutdown signal or server error -------------------------
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-serverErr:
		log.Error("server error", zap.Error(err))
		stop()
	}

	// ---- Graceful shutdown ------------------------------------------------
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Info("shutting down HTTP server...")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("server shutdown error", zap.Error(err))
	}

	log.Info("waiting for in-flight jobs to finish...")
	wg.Wait()

	log.Info("shutdown complete")
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok","service":"whatsapp-bot-go"}`)) //nolint:errcheck
}
