// Command query-service serves object reads (Spec §8.3) with permissions and redaction.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/auth"
	"github.com/acme/ontology/internal/authz"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/oms"
	"github.com/acme/ontology/internal/omsclient"
	"github.com/acme/ontology/internal/query"
)

func main() {
	addr := envOr("ONTO_SERVER_ADDR", ":8082")
	dsn := mustEnv("ONTO_DB_DSN")
	ontology := envOr("ONTO_ONTOLOGY", "mro")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fatal("connect db", err)
	}
	defer pool.Close()

	loader := omsclient.NewHTTPLoader(envOr("ONTO_OMS_URL", "http://localhost:8080"), ontology)
	loader.Token = os.Getenv("ONTO_OMS_TOKEN")
	loader.Actor = envOr("ONTO_OMS_ACTOR", "query-service")
	cache := omsclient.New(loader, 3)
	go oms.ListenVersions(ctx, func(c context.Context) (*pgx.Conn, error) { return pgx.Connect(c, dsn) }, ontology, cache)

	authn, _, err := auth.FromEnv(ctx)
	if err != nil {
		fatal("authentication setup", err)
	}
	clearances, err := authz.ParseClearances(os.Getenv("ONTO_CLEARANCE_GROUPS"))
	if err != nil {
		fatal("ONTO_CLEARANCE_GROUPS", err)
	}
	var az authz.Authorizer
	switch {
	case os.Getenv("ONTO_FGA_URL") != "":
		az = authz.NewOpenFGA(os.Getenv("ONTO_FGA_URL"), mustEnv("ONTO_FGA_STORE_ID"), os.Getenv("ONTO_FGA_MODEL_ID"))
	case os.Getenv("ONTO_AUTHZ_ALLOW_ALL") == "true":
		slog.Warn("authorization disabled (ONTO_AUTHZ_ALLOW_ALL=true); development only")
		az = authz.AllowAll{}
	default:
		slog.Error("set ONTO_FGA_URL (or ONTO_AUTHZ_ALLOW_ALL=true for local dev)")
		os.Exit(1)
	}

	h := &query.Handler{
		Service:  &query.Service{Ontologies: cache, Store: objstore.New(pool), Authz: az, Clearances: clearances},
		Ontology: ontology,
		Auth:     authn,
	}
	srv := &http.Server{Addr: addr, Handler: h.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("query-service listening", "addr", addr, "ontology", ontology)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("missing required environment variable", "name", k)
		os.Exit(1)
	}
	return v
}
