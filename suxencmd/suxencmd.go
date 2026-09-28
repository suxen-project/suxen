// Package suxencmd runs the suxen artifact repository server.
//
// It is importable so custom distributions can compile in additional plugins:
// a third-party main package blank-imports its plugin packages (which register
// drivers and formats through the spi packages) and calls Main.
//
//	package main
//
//	import (
//		_ "example.com/my/suxen-plugin"
//		_ "github.com/suxen-project/suxen/plugins/builtin"
//
//		"github.com/suxen-project/suxen/suxencmd"
//	)
//
//	func main() { suxencmd.Main() }
package suxencmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/suxen-project/suxen/internal/blob" // register the fs and s3 drivers
	"github.com/suxen-project/suxen/internal/config"
	"github.com/suxen-project/suxen/internal/registry"
	"github.com/suxen-project/suxen/internal/server"
	"github.com/suxen-project/suxen/internal/startup"
	"github.com/suxen-project/suxen/internal/store"
	spiblob "github.com/suxen-project/suxen/spi/blob"
)

// Main parses the command line and runs the server. It exits the process on
// failure.
func Main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(server.Version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: suxen [serve|version]")
		os.Exit(2)
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "suxen:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	logger := newLogger(cfg.LogLevel)
	registerBuiltInDrivers()
	metadata, err := openMetadata(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer metadata.Close()

	blobStore, err := openBlobStore(cfg.BlobURL)
	if err != nil {
		return err
	}

	handler := server.New(cfg, metadata, blobStore, logger)
	defer handler.Close()

	primaryPort, err := listenPort(cfg.Listen)
	if err != nil {
		return err
	}

	// Serve health and readiness before any dependency-backed startup work.
	// While initialization runs and retries, readiness reports 503 so the pod
	// stays out of rotation, but the process and /healthz stay up — an
	// unavailable database or blob store never restarts the pod.
	handler.SetInitializing(true)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler.HandlerForPort(primaryPort),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       2 * time.Minute,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("server listening", "address", cfg.Listen, "data", cfg.DataDir)
		serverErrors <- httpServer.ListenAndServe()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	schedulerContext, stopScheduler := context.WithCancel(context.Background())
	defer stopScheduler()

	switch ready, initErr := initialize(handler, cfg, logger, signals, serverErrors); {
	case initErr != nil:
		_ = shutdownHTTPServer(httpServer, 15*time.Second)
		return initErr
	case !ready:
		// A shutdown signal arrived before initialization completed.
		return shutdownHTTPServer(httpServer, 15*time.Second)
	}
	handler.StartScheduler(schedulerContext)
	handler.SetInitializing(false)

	select {
	case received := <-signals:
		logger.Info("shutting down", "signal", received.String())
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case err := <-handler.ExtraListenerErrors():
		return err
	}
	stopScheduler()

	return shutdownHTTPServer(httpServer, 15*time.Second)
}

// initialize runs the database- and blob-store-backed startup steps, retrying
// transient failures with capped backoff until they succeed. It returns
// (true, nil) once startup completes, (false, nil) if a shutdown signal arrives
// first, and (false, err) if the primary listener fails fatally. Retrying rather
// than exiting is what keeps a dependency outage from restarting the pod: the
// process stays alive and NotReady until the dependency recovers.
func initialize(
	handler *server.Server,
	cfg config.Config,
	logger *slog.Logger,
	signals <-chan os.Signal,
	serverErrors <-chan error,
) (bool, error) {
	return initializeWithAttempt(func() error {
		return initializeOnce(handler, cfg, logger)
	}, logger, signals, serverErrors, time.Second, 30*time.Second)
}

func initializeWithAttempt(
	attemptOnce func() error,
	logger *slog.Logger,
	signals <-chan os.Signal,
	serverErrors <-chan error,
	initialBackoff, maxBackoff time.Duration,
) (bool, error) {
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		err := attemptOnce()
		if err == nil {
			return true, nil
		}
		if !startup.ShouldRetry(err) {
			return false, err
		}
		logger.Error(
			"startup initialization failed; retrying",
			"attempt", attempt,
			"retryIn", backoff.String(),
			"error", err,
		)
		timer := time.NewTimer(backoff)
		select {
		case received := <-signals:
			timer.Stop()
			logger.Info("shutting down during initialization", "signal", received.String())
			return false, nil
		case err := <-serverErrors:
			timer.Stop()
			if errors.Is(err, http.ErrServerClosed) {
				return false, nil
			}
			return false, err
		case <-timer.C:
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// initializeOnce performs one attempt at the dependency-backed startup steps.
// Every step is idempotent, so a caller may retry marked dependency failures.
func initializeOnce(handler *server.Server, cfg config.Config, logger *slog.Logger) error {
	ctx := context.Background()
	bootstrapMessage, err := handler.Bootstrap(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if bootstrapMessage != "" {
		logger.Warn(bootstrapMessage)
	}
	if cfg.Provision != "" {
		report, applied, err := handler.ApplyProvisionSource(ctx, cfg.Provision)
		if err != nil {
			return fmt.Errorf("apply startup provisioning: %w", err)
		}
		if applied {
			logger.Info("startup provisioning completed", "results", report.Results)
			if report.Failed() {
				return errors.New("startup provisioning contains failed resources")
			}
		} else {
			logger.Info("startup provisioning already completed by another replica")
		}
	}
	if err := handler.ValidateOperationalConfiguration(ctx); err != nil {
		return fmt.Errorf("validate operational configuration: %w", err)
	}
	if err := handler.StartExtraListeners(ctx); err != nil {
		return fmt.Errorf("start extra listeners: %w", err)
	}
	return nil
}

func shutdownHTTPServer(httpServer *http.Server, timeout time.Duration) error {
	shutdownContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return httpServer.Shutdown(shutdownContext)
}

func listenPort(addr string) (int, error) {
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("parse listen address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid listen port in %q", addr)
	}
	return port, nil
}

func registerBuiltInDrivers() {
	registerDriversOnce.Do(func() {
		registry.RegisterStore("sqlite", func(path string) (store.Store, error) {
			return store.OpenSQLite(path)
		})
		registry.RegisterStore("postgres", func(dataSourceName string) (store.Store, error) {
			return store.OpenPostgres(dataSourceName)
		})
	})
}

var registerDriversOnce sync.Once

func openMetadata(databaseURL string) (store.Store, error) {
	if strings.HasPrefix(databaseURL, "sqlite://") {
		databasePath := strings.TrimPrefix(databaseURL, "sqlite://")
		if err := os.MkdirAll(filepath.Dir(databasePath), 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
		return registry.Store("sqlite", databasePath)
	}
	if strings.HasPrefix(databaseURL, "postgres://") ||
		strings.HasPrefix(databaseURL, "postgresql://") {
		return registry.Store("postgres", databaseURL)
	}
	return nil, fmt.Errorf("unsupported metadata URL %q", databaseURL)
}

func openBlobStore(blobURL string) (spiblob.Store, error) {
	driver, found := spiblob.ForURL(blobURL)
	if !found {
		return nil, fmt.Errorf("unsupported blob store URL %q", blobURL)
	}
	blobStore, err := driver.Open(blobURL)
	if err != nil {
		return nil, fmt.Errorf("open %s blob store: %w", driver.Name, err)
	}
	return blobStore, nil
}

func newLogger(levelName string) *slog.Logger {
	return newLoggerWithOutput(levelName, os.Stdout)
}

func newLoggerWithOutput(levelName string, output io.Writer) *slog.Logger {
	level := slog.LevelInfo
	parseErr := level.UnmarshalText([]byte(levelName))
	if parseErr != nil {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))
	if parseErr != nil {
		logger.Warn(
			"invalid log level; using info",
			"configured_level", levelName,
			"error", parseErr,
		)
	}
	return logger
}
