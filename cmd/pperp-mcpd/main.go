// Command pperp-mcpd is the hosted MCP server of Phoenix Perpetuals (role mcp on the API box, behind nginx at
// mcp.phoenixperpsfunding.com). Its settings come from the environment:
//
//	MCP_LISTEN          127.0.0.1:7104
//	MCP_GATEWAY_SOCKET  /run/perps-api/gateway.sock (the gateway's door for this role)
//	MCP_PUBLIC_URL      https://mcp.phoenixperpsfunding.com
//	MCP_AUTH_SERVER     https://phoenixperpsfunding.com
//	MCP_DOCS_URL        https://docs.phoenixperpsfunding.com/mcp
//	MCP_LIVE_HOST, MCP_SANDBOX_HOST   the REST host names (the environment of each call)
//	MCP_CONFIRM_SECRET  the key previews' confirmations are signed with (hex, 32 bytes or more)
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/phoenixperps/pperp/internal/mcp"
)

var version = "dev" // set at build: -ldflags "-X main.version=..."

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	secret, err := hex.DecodeString(os.Getenv("MCP_CONFIRM_SECRET"))
	if err != nil || len(secret) < 32 {
		log.Error("MCP_CONFIRM_SECRET: 32 bytes or more, in hex")
		os.Exit(1)
	}
	hosts := map[string]string{
		"live":    env("MCP_LIVE_HOST", "api.phoenixperpsfunding.com"),
		"sandbox": env("MCP_SANDBOX_HOST", "sandbox-api.phoenixperpsfunding.com"),
	}
	gw := mcp.NewGateway(env("MCP_GATEWAY_SOCKET", "/run/perps-api/gateway.sock"), hosts, "pperp-mcpd/"+version)
	h := mcp.New(mcp.Config{
		PublicURL:  env("MCP_PUBLIC_URL", "https://mcp.phoenixperpsfunding.com"),
		AuthServer: env("MCP_AUTH_SERVER", "https://phoenixperpsfunding.com"),
		DocsURL:    env("MCP_DOCS_URL", "https://docs.phoenixperpsfunding.com/mcp"),
		Version:    version,
	}, gw, mcp.NewConfirmations(secret, nil))
	srv := &http.Server{Addr: env("MCP_LISTEN", "127.0.0.1:7104"), Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Info("pperp-mcpd: listening", "addr", srv.Addr, "version", version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("pperp-mcpd", "error", err.Error())
		os.Exit(1)
	}
}
