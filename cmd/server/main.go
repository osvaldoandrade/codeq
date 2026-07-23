package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof" // #nosec G108 -- opt-in pprof uses a separate loopback listener by default.
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	_ "github.com/osvaldoandrade/codeq/pkg/auth/jwks"   // Register default JWKS auth provider
	_ "github.com/osvaldoandrade/codeq/pkg/auth/multi"  // Register bounded multi-provider auth
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static" // Register static token auth provider (dev/local)
	"github.com/osvaldoandrade/codeq/pkg/config"
	_ "github.com/osvaldoandrade/codeq/pkg/persistence/memory" // Register memory persistence plugin (testing)
	_ "github.com/osvaldoandrade/codeq/pkg/persistence/redis"  // Register Redis persistence plugin

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

	// Optional pprof endpoint on a separate listener so it never competes with
	// the API server's request handlers. Set CODEQ_PPROF=1 to enable; bind addr
	// configurable via CODEQ_PPROF_ADDR (default 127.0.0.1:6060). Mutex/block profiling
	// rates are also enabled so contention shows up in samples.
	if getenv("CODEQ_PPROF", "") == "1" {
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		pprofAddr := getenv("CODEQ_PPROF_ADDR", "127.0.0.1:6060")
		go func() {
			pprofSrv := &http.Server{Addr: pprofAddr, ReadHeaderTimeout: 5 * time.Second}
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "[WARN] pprof server:", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	// Best-effort flush of trace exporter (if enabled).
	if application.TracingShutdown != nil {
		traceCtx, traceCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer traceCancel()
		_ = application.TracingShutdown(traceCtx)
	}
}
