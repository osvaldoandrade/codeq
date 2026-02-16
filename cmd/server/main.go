package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/jwks"   // Register default JWKS auth provider
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static" // Register static token auth provider (dev/local)
	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/osvaldoandrade/codeq/pkg/app"
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfgPath := getenv("CODEQ_CONFIG_PATH", "")

	cfg, err := config.LoadConfigOptional(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[ERROR] load config:", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "[ERROR] invalid config:", err)
		os.Exit(1)
	}

	application, err := app.NewApplication(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[ERROR] init app:", err)
		os.Exit(1)
	}
	app.SetupMappings(application)

	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           application.Engine,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "[ERROR] http server:", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	// Best-effort flush of trace exporter (if enabled).
	if application.TracingShutdown != nil {
		_ = application.TracingShutdown(ctx)
	}
}
