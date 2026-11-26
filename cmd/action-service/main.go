// Command action-service runs the Action Service HTTP API and its Temporal worker (Spec §7, §13).
//
//	--role=api     HTTP only (starts workflows, does not execute them)
//	--role=worker  Temporal worker only
//	--role=all     both (default; convenient for local development)
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/acme/ontology/internal/action"
	"github.com/acme/ontology/internal/action/functions"
	"github.com/acme/ontology/internal/action/workflow"
	"github.com/acme/ontology/internal/auth"
	"github.com/acme/ontology/internal/authz"
	"github.com/acme/ontology/internal/objstore"
	"github.com/acme/ontology/internal/oms"
	"github.com/acme/ontology/internal/omsclient"
	"github.com/acme/ontology/internal/webhook"
)

func main() {
	role := flag.String("role", "all", "api | worker | all")
	flag.Parse()
	runAPI := *role == "api" || *role == "all"
	runWorker := *role == "worker" || *role == "all"
	if !runAPI && !runWorker {
		slog.Error("invalid --role", "role", *role)
		os.Exit(2)
	}

	addr := envOr("ONTO_SERVER_ADDR", ":8081")
	dsn := mustEnv("ONTO_DB_DSN")
	omsURL := envOr("ONTO_OMS_URL", "http://localhost:8080")
	ontology := envOr("ONTO_ONTOLOGY", "mro")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		slog.Error("connect db", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	loader := omsclient.NewHTTPLoader(omsURL, ontology)
	loader.Token = os.Getenv("ONTO_OMS_TOKEN") // service token for reading metadata from OMS
	loader.Actor = envOr("ONTO_OMS_ACTOR", "action-service")
	cache := omsclient.New(loader, 3)
	go oms.ListenVersions(ctx, func(c context.Context) (*pgx.Conn, error) { return pgx.Connect(c, dsn) }, ontology, cache)

	store := objstore.New(pool)
	committer := action.NewPGCommitter(pool, store)

	tc, err := client.Dial(client.Options{
		HostPort:  envOr("ONTO_TEMPORAL_HOSTPORT", "localhost:7233"),
		Namespace: envOr("ONTO_TEMPORAL_NAMESPACE", "default"),
	})
	if err != nil {
		slog.Error("connect temporal", "err", err)
		os.Exit(1)
	}
	defer tc.Close()

	if runWorker {
		w := worker.New(tc, workflow.TaskQueue, worker.Options{})
		w.RegisterWorkflow(workflow.ActionWorkflow)
		w.RegisterWorkflow(workflow.SideEffectsWorkflow)
		w.RegisterActivity(&workflow.Activities{
			Store:      committer,
			Ontologies: cache,
			Webhooks:   webhook.NewExecutor(mustSecrets(), splitList(os.Getenv("ONTO_WEBHOOK_ALLOWED_HOSTS"))),
			Notifier:   action.LogNotifier{},
		})
		if err := w.Start(); err != nil {
			slog.Error("start worker", "err", err)
			os.Exit(1)
		}
		defer w.Stop()
		slog.Info("temporal worker started", "taskQueue", workflow.TaskQueue)
		if iv := os.Getenv("ONTO_SWEEP_INTERVAL"); iv != "off" {
			sw := &action.Sweeper{Store: committer, Dispatcher: &workflow.Dispatcher{Client: tc},
				Interval:  envDuration("ONTO_SWEEP_INTERVAL", time.Minute),
				OlderThan: envDuration("ONTO_SWEEP_OLDER_THAN", 5*time.Minute)}
			go sw.Run(ctx)
			slog.Info("stuck-run sweeper started", "interval", sw.Interval, "olderThan", sw.OlderThan)
		}
	}

	if runAPI {
		az := mustAuthorizer()
		authn, _, err := auth.FromEnv(ctx)
		if err != nil {
			slog.Error("authentication setup", "err", err)
			os.Exit(1)
		}
		registry := action.NewFunctionRegistry()
		functions.Register(registry)
		runtimes, callbacks := mustRuntimes()
		h := &action.Handler{
			Engine: &action.Engine{Ontologies: cache, Objects: store, Authz: az, Committer: committer,
				Functions: registry, Dispatcher: &workflow.Dispatcher{Client: tc},
				SyncWait: envDuration("ONTO_ACTION_SYNC_WAIT", 10*time.Second), Runtimes: runtimes},
			Committer: committer,
			Ontology:  ontology,
			Identity:  action.IdentityFrom(authn),
			MaxBatch:  100,
			Audit:     committer,
			Limiter:   mustLimiter(),
		}
		h.AuditGroups = splitList(os.Getenv("ONTO_AUDIT_GROUPS"))
		routes := http.NewServeMux()
		routes.Handle("/", h.Routes())
		if callbacks != nil {
			// Read callbacks for TypeScript/Python functions. Each path carries a random per-invocation
			// token; expose this prefix only on the network the runtimes use.
			routes.Handle("/internal/function-reads/", http.StripPrefix("/internal/function-reads", callbacks.Handler()))
		}
		srv := &http.Server{Addr: addr, Handler: routes, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			slog.Info("action-service listening", "addr", addr, "ontology", ontology)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("server error", "err", err)
				stop()
			}
		}()
		defer func() {
			shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(shut)
		}()
	}
	<-ctx.Done()
}

func mustAuthorizer() authz.Authorizer {
	switch {
	case os.Getenv("ONTO_FGA_URL") != "":
		return authz.NewOpenFGA(os.Getenv("ONTO_FGA_URL"), mustEnv("ONTO_FGA_STORE_ID"), os.Getenv("ONTO_FGA_MODEL_ID"))
	case os.Getenv("ONTO_AUTHZ_ALLOW_ALL") == "true":
		slog.Warn("authorization disabled (ONTO_AUTHZ_ALLOW_ALL=true); development only")
		return authz.AllowAll{}
	}
	slog.Error("set ONTO_FGA_URL (or ONTO_AUTHZ_ALLOW_ALL=true for local dev)")
	os.Exit(1)
	return nil
}

// mustSecrets resolves env:// always and vault:// when VAULT_ADDR is set.
func mustSecrets() webhook.SecretProvider {
	v, err := webhook.VaultFromEnv()
	if err != nil {
		slog.Error("vault configuration", "err", err)
		os.Exit(1)
	}
	if v == nil {
		return webhook.Secrets{}
	}
	slog.Info("vault secrets enabled", "addr", v.Addr)
	return webhook.Secrets{Vault: v}
}

// mustRuntimes reads ONTO_FUNCTION_RUNTIMES ("python=<url>,typescript=<url>"), ONTO_FUNCTION_CALLBACK_URL
// (this service as the runtimes reach it), ONTO_FUNCTION_RUNTIME_TOKEN and ONTO_FUNCTION_TIMEOUT.
func mustRuntimes() (map[string]*action.RemoteRuntime, *action.FunctionCallbacks) {
	urls, err := action.ParseRuntimes(os.Getenv("ONTO_FUNCTION_RUNTIMES"))
	if err != nil {
		slog.Error("ONTO_FUNCTION_RUNTIMES", "err", err)
		os.Exit(1)
	}
	if len(urls) == 0 {
		return nil, nil
	}
	cb := action.NewFunctionCallbacks()
	out := map[string]*action.RemoteRuntime{}
	for name, u := range urls {
		out[name] = &action.RemoteRuntime{Name: name, URL: u, CallbackURL: mustEnv("ONTO_FUNCTION_CALLBACK_URL"),
			Token: os.Getenv("ONTO_FUNCTION_RUNTIME_TOKEN"), Timeout: envDuration("ONTO_FUNCTION_TIMEOUT", 10*time.Second),
			Callbacks: cb}
		slog.Info("function runtime configured", "runtime", name, "url", u)
	}
	return out, cb
}

// mustLimiter reads ONTO_RATE_LIMIT ("<perMinute>[/<burst>]", default unlimited),
// ONTO_RATE_LIMIT_AGENT (same format, for AGENT callers) and ONTO_RATE_LIMIT_ACTIONS
// ("<action>=<perMinute>[/<burst>],..."). It returns nil when no limit is set.
func mustLimiter() *action.Limiter {
	def, err := action.ParseRateLimit(os.Getenv("ONTO_RATE_LIMIT"))
	if err != nil {
		slog.Error("ONTO_RATE_LIMIT", "err", err)
		os.Exit(1)
	}
	per, err := action.ParseActionRateLimits(os.Getenv("ONTO_RATE_LIMIT_ACTIONS"))
	if err != nil {
		slog.Error("ONTO_RATE_LIMIT_ACTIONS", "err", err)
		os.Exit(1)
	}
	l := &action.Limiter{Default: def, PerAction: per}
	if v := os.Getenv("ONTO_RATE_LIMIT_AGENT"); v != "" {
		rl, err := action.ParseRateLimit(v)
		if err != nil {
			slog.Error("ONTO_RATE_LIMIT_AGENT", "err", err)
			os.Exit(1)
		}
		l.Agents = &rl
	}
	if def.PerMinute <= 0 && len(per) == 0 && l.Agents == nil {
		return nil
	}
	return l
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envDuration(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if p, err := time.ParseDuration(v); err == nil {
			return p
		}
	}
	return d
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
