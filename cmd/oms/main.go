// Command oms runs the Ontology Metadata Service.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/auth"
	"github.com/acme/ontology/internal/oms"
)

func main() {
	addr := envOr("ONTO_SERVER_ADDR", ":8080")
	dsn := os.Getenv("ONTO_DB_DSN")
	if dsn == "" {
		slog.Error("ONTO_DB_DSN is required")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		slog.Error("connect db", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	authn, _, err := auth.FromEnv(ctx)
	if err != nil {
		slog.Error("authentication setup", "err", err)
		os.Exit(1)
	}
	owners := auth.SplitList(os.Getenv("ONTO_OMS_OWNER_GROUPS"))
	if len(owners) == 0 {
		slog.Warn("ONTO_OMS_OWNER_GROUPS is empty: any authenticated caller can publish and activate versions")
	}

	store := oms.NewStore(pool)
	svc := oms.NewService(store)
	h := oms.NewHandler(svc, store, authn, owners)

	srv := &http.Server{
		Addr:              addr,
		Handler:           h.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("oms listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	slog.Info("oms stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
