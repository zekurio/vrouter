// Command vrouter runs the model gateway and serves its web UI.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
		DataDir:         os.Getenv("VROUTER_DATA_DIR"),
		PublicURL:       os.Getenv("VROUTER_PUBLIC_URL"),
		StartWindows:    true,
		WindowSkipPlans: os.Getenv("VROUTER_WINDOW_SKIP_PLANS"),
		PriceCatalog:    os.Getenv("VROUTER_PRICE_CATALOG"),
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
	key, generated, err := adminKey()
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	cfg.AdminKey = key
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
	if generated {
		// Standard error, not the structured log, so the key is easy to spot
		// and copy.
		fmt.Fprintf(os.Stderr, "\nNo VROUTER_ADMIN_KEY is set. Sign in to the dashboard with this key, which lasts until vrouter stops:\n\n    %s\n\n", key)
	}
	if err := serve(addr, handler); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// adminKey reads the admin key from VROUTER_ADMIN_KEY or the file
// VROUTER_ADMIN_KEY_FILE names. With neither set it makes a key for this run
// and reports that it did.
func adminKey() (string, bool, error) {
	key, path := os.Getenv("VROUTER_ADMIN_KEY"), os.Getenv("VROUTER_ADMIN_KEY_FILE")
	if key != "" && path != "" {
		return "", false, errors.New("set VROUTER_ADMIN_KEY or VROUTER_ADMIN_KEY_FILE, not both")
	}
	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // the operator names the key file
		if err != nil {
			return "", false, fmt.Errorf("read VROUTER_ADMIN_KEY_FILE: %w", err)
		}
		return strings.TrimSpace(string(data)), false, nil
	}
	if key != "" {
		return key, false, nil
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", false, err
	}
	return "vra_" + base64.RawURLEncoding.EncodeToString(raw[:]), true, nil
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
