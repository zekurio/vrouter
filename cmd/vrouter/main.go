// Command vrouter runs the model gateway and serves its web UI.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"vrouter/internal/gateway"
	"vrouter/web"
)

func main() {
	cfg := gateway.Config{
		DataDir:   os.Getenv("VROUTER_DATA_DIR"),
		PublicURL: os.Getenv("VROUTER_PUBLIC_URL"),
		OIDC: gateway.OIDCConfig{
			Issuer:   os.Getenv("VROUTER_OIDC_ISSUER"),
			ClientID: os.Getenv("VROUTER_OIDC_CLIENT_ID"),
			Groups:   os.Getenv("VROUTER_OIDC_GROUPS"),
		},
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
	if _, _, err := net.SplitHostPort(addr); err != nil {
		slog.Error("invalid listen address", "error", err)
		os.Exit(1)
	}
	for _, name := range []string{"VROUTER_ADMIN_TOKEN", "VROUTER_EXTERNAL_AUTH"} {
		if os.Getenv(name) != "" {
			slog.Error(name + " was removed; people now sign in through OpenID Connect. Unset it and set the VROUTER_OIDC_ variables instead.")
			os.Exit(1)
		}
	}
	secret, err := clientSecret()
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	cfg.OIDC.ClientSecret = secret
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
	if err := serve(addr, handler); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// clientSecret reads the OIDC client secret from VROUTER_OIDC_CLIENT_SECRET or
// the file VROUTER_OIDC_CLIENT_SECRET_FILE names. A public client has none.
func clientSecret() (string, error) {
	secret, path := os.Getenv("VROUTER_OIDC_CLIENT_SECRET"), os.Getenv("VROUTER_OIDC_CLIENT_SECRET_FILE")
	if path == "" {
		return secret, nil
	}
	if secret != "" {
		return "", errors.New("set VROUTER_OIDC_CLIENT_SECRET or VROUTER_OIDC_CLIENT_SECRET_FILE, not both")
	}
	data, err := os.ReadFile(path) //nolint:gosec // the operator names the secret file
	if err != nil {
		return "", fmt.Errorf("read VROUTER_OIDC_CLIENT_SECRET_FILE: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func serve(addr string, handler http.Handler) error {
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
		return err
	}
	<-shutdownDone
	return nil
}
