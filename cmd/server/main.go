package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"wallet-service/internal/config"
	"wallet-service/internal/handler"
	"wallet-service/internal/repository"
	"wallet-service/internal/service"
)

func main() {
	config.LoadDotEnv() // reads .env (real environment variables win)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := config.Getenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/wallet?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping db (is `docker compose up -d` running?): %v", err)
	}
	if err := repository.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	svc := service.New(repository.NewStore(pool))
	srv := &http.Server{
		Addr:              config.Getenv("HTTP_ADDR", ":8080"),
		Handler:           handler.New(svc).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
