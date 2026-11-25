// Command mcp-server exposes the ontology to AI agents over MCP (revision 2026-07-28,
// stateless Streamable HTTP at /mcp). It holds no data access of its own: every tool call is
// forwarded to query-service / action-service with the caller's credentials.
package main

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/acme/ontology/internal/auth"
	"github.com/acme/ontology/internal/mcp"
	"github.com/acme/ontology/internal/omsclient"
)

var version = "dev"

func main() {
	addr := envOr("ONTO_SERVER_ADDR", "127.0.0.1:8083") // spec: bind to localhost when run locally
	ontology := envOr("ONTO_ONTOLOGY", "mro")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	authn, _, err := auth.FromEnv(ctx)
	if err != nil {
		fatal("authentication setup", err)
	}
	secret, err := hex.DecodeString(os.Getenv("ONTO_MCP_CONFIRM_SECRET"))
	if err != nil || len(secret) < 16 {
		slog.Warn("ONTO_MCP_CONFIRM_SECRET is not a hex string of at least 16 bytes; actions that need human confirmation will be refused")
		secret = nil
	}
	loader := omsclient.NewHTTPLoader(envOr("ONTO_OMS_URL", "http://localhost:8080"), ontology)
	loader.Token = os.Getenv("ONTO_OMS_TOKEN")
	loader.Actor = envOr("ONTO_OMS_ACTOR", "mcp-server")

	s := &mcp.Server{
		Ontologies: omsclient.New(loader, 2),
		Backend: &mcp.Backend{
			QueryURL:  envOr("ONTO_QUERY_URL", "http://localhost:8082"),
			ActionURL: envOr("ONTO_ACTION_URL", "http://localhost:8081"),
			Ontology:  ontology,
		},
		Auth:           authn,
		AllowedOrigins: auth.SplitList(os.Getenv("ONTO_MCP_ALLOWED_ORIGINS")),
		ConfirmSecret:  secret,
		Name:           "ontology-mcp",
		Version:        version,
	}
	srv := &http.Server{Addr: addr, Handler: s.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("mcp-server listening", "addr", addr, "endpoint", "/mcp", "protocol", mcp.ProtocolVersion)
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
