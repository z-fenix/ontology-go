// Command fga-sync keeps OpenFGA relationship tuples in line with object data (Spec §8.3).
//
//	fga-sync               run continuously: outbox every ONTO_FGA_SYNC_INTERVAL (default 5s),
//	                       full reconcile every ONTO_FGA_FULL_INTERVAL (default 10m)
//	fga-sync --once        one full reconcile of every authz-bound type, then exit
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/authz"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/omsclient"
	"github.com/acme/ontology/internal/spec"
)

func main() {
	once := flag.Bool("once", false, "run one full reconcile and exit")
	flag.Parse()
	if err := run(*once); err != nil {
		slog.Error("fga-sync", "err", err)
		os.Exit(1)
	}
}

func run(once bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dsn, fgaURL, store := os.Getenv("ONTO_DB_DSN"), os.Getenv("ONTO_FGA_URL"), os.Getenv("ONTO_FGA_STORE_ID")
	if dsn == "" || fgaURL == "" || store == "" {
		return fmt.Errorf("ONTO_DB_DSN, ONTO_FGA_URL and ONTO_FGA_STORE_ID are required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	loader := omsclient.NewHTTPLoader(envOr("ONTO_OMS_URL", "http://localhost:8080"), envOr("ONTO_ONTOLOGY", "mro"))
	loader.Token = os.Getenv("ONTO_OMS_TOKEN")
	loader.Actor = envOr("ONTO_OMS_ACTOR", "fga-sync")
	cache := omsclient.New(loader, 2)
	s := &authz.FGASync{DB: pool, Store: objstore.New(pool), Writer: authz.NewOpenFGA(fgaURL, store, os.Getenv("ONTO_FGA_MODEL_ID"))}

	if once {
		ont, err := cache.Active(ctx)
		if err != nil {
			return err
		}
		types := make([]string, 0, len(ont.ObjectTypes))
		for name, ot := range ont.ObjectTypes {
			if ot.Authz != nil {
				types = append(types, name)
			}
		}
		sort.Strings(types)
		for _, typ := range types {
			w, d, err := s.FullSync(ctx, ont, typ, 0)
			if err != nil {
				return fmt.Errorf("%s: %w", typ, err)
			}
			fmt.Printf("%s: %d written, %d deleted\n", typ, w, d)
		}
		return nil
	}
	slog.Info("fga-sync started")
	s.Run(ctx, envDuration("ONTO_FGA_SYNC_INTERVAL", 5*time.Second), envDuration("ONTO_FGA_FULL_INTERVAL", 10*time.Minute),
		func(ctx context.Context) (*spec.CompiledOntology, error) { return cache.Active(ctx) })
	return nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envDuration(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if p, err := time.ParseDuration(v); err == nil {
			return p
		}
	}
	return d
}
