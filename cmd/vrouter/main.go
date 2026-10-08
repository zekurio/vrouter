package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"vrouter/internal/gateway"
	"vrouter/web"
)

func main() {
	cfg := gateway.Config{
		DataDir:         os.Getenv("VROUTER_DATA_DIR"),
		AdminToken:      os.Getenv("VROUTER_ADMIN_TOKEN"),
		PublicURL:       os.Getenv("VROUTER_PUBLIC_URL"),
		ExternalAuth:    os.Getenv("VROUTER_EXTERNAL_AUTH") == "1",
		StartWindows:    true,
		WindowSkipPlans: os.Getenv("VROUTER_WINDOW_SKIP_PLANS"),
	}
	if len(os.Args) > 1 {
		if os.Args[1] != "import" || len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: vrouter [import credential.json ...]")
			os.Exit(2)
		}
		dir := cfg.DataDir
		if dir == "" {
			dir = gateway.DefaultDataDir()
		}
		imported, skipped, err := gateway.ImportCredentials(dir, os.Args[2:])
		if err != nil {
			slog.Error("credential import failed", "error", err)
			os.Exit(1)
		}
		fmt.Printf("Imported %d accounts; skipped %d existing accounts.\n", imported, skipped)
		return
	}
	addr := os.Getenv("VROUTER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		slog.Error("invalid listen address", "error", err)
		os.Exit(1)
	}
	ip := net.ParseIP(host)
	if (ip == nil || !ip.IsLoopback()) && cfg.AdminToken == "" && !cfg.ExternalAuth {
		slog.Error("VROUTER_ADMIN_TOKEN or VROUTER_EXTERNAL_AUTH=1 is required when listening outside loopback")
		os.Exit(1)
	}
	assets := web.Assets()
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		slog.Error("frontend build missing; run pnpm --dir web install --frozen-lockfile and pnpm --dir web build before building Go")
		os.Exit(1)
	}
	handler, err := gateway.New(cfg, assets)
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	if closer, ok := handler.(io.Closer); ok {
		defer closer.Close()
	}
	server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("vrouter listening", "address", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
	<-shutdownDone
}
