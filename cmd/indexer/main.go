// Command indexer syncs datasource rows into the ontology's object tables (Spec §5).
//
//	indexer                    run continuously (poll every ONTO_INDEXER_INTERVAL, default 30s)
//	indexer --once             one round, then exit (cron / CI)
//	indexer --once --full      force full snapshots (catches hard deletes)
//	indexer --force            allow a full sync to delete more than half of a type
//
// Source connections come from ONTO_SOURCE_<CONNECTOR>_DSN (connector name upper-cased),
// e.g. ONTO_SOURCE_MRO_DB_DSN for connector "mro_db". The ontology is read from OMS.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/ontology/internal/indexer"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/omsclient"
	"github.com/acme/ontology/internal/spec"
)

func main() {
	once := flag.Bool("once", false, "run one sync round and exit")
	full := flag.Bool("full", false, "force full snapshots in this run")
	force := flag.Bool("force", false, "disable the mass-delete guard")
	flag.Parse()
	if err := run(*once, *full, *force); err != nil {
		slog.Error("indexer", "err", err)
		os.Exit(1)
	}
}

func run(once, full, force bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("ONTO_DB_DSN")
	if dsn == "" {
		return fmt.Errorf("ONTO_DB_DSN is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	loader := omsclient.NewHTTPLoader(envOr("ONTO_OMS_URL", "http://localhost:8080"), envOr("ONTO_ONTOLOGY", "mro"))
	loader.Token = os.Getenv("ONTO_OMS_TOKEN")
	loader.Actor = envOr("ONTO_OMS_ACTOR", "indexer")
	cache := omsclient.New(loader, 2)

	ont, err := cache.Active(ctx)
	if err != nil {
		return fmt.Errorf("load ontology from OMS: %w", err)
	}
	sources, closeSources, err := openSources(ctx, ont)
	if err != nil {
		return err
	}
	defer closeSources()

	ix := &indexer.Indexer{DB: pool, Store: objstore.New(pool), Sources: sources, Force: force,
		FullInterval: envDuration("ONTO_INDEXER_FULL_INTERVAL", indexer.DefaultFullInterval)}

	if once {
		results := ix.SyncAll(ctx, ont, full)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		for _, r := range results {
			if r.Error != "" {
				return fmt.Errorf("%s failed: %s", r.Source, r.Error)
			}
		}
		return nil
	}
	interval := envDuration("ONTO_INDEXER_INTERVAL", 30*time.Second)
	slog.Info("indexer started", "interval", interval, "ontology", ont.APIName)
	ix.Run(ctx, interval, func(ctx context.Context) (*spec.CompiledOntology, error) { return cache.Active(ctx) })
	return nil
}

func openSources(ctx context.Context, ont *spec.CompiledOntology) (map[string]indexer.Querier, func(), error) {
	out := map[string]indexer.Querier{}
	var pools []*pgxpool.Pool
	closeAll := func() {
		for _, p := range pools {
			p.Close()
		}
	}
	for name, c := range ont.Connectors {
		env := "ONTO_SOURCE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_DSN"
		dsn := os.Getenv(env)
		if dsn == "" {
			slog.Warn("connector has no source DSN; its object types are skipped", "connector", name, "env", env)
			continue
		}
		if c.Kind != "" && c.Kind != "postgres" {
			closeAll()
			return nil, nil, fmt.Errorf("connector %s: kind %q is not supported (postgres only)", name, c.Kind)
		}
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("connector %s: %w", name, err)
		}
		pools = append(pools, p)
		out[name] = p
	}
	return out, closeAll, nil
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
