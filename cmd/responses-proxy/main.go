package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"openai-responses-proxy/internal/accounting"
	"openai-responses-proxy/internal/admin"
	"openai-responses-proxy/internal/auth"
	"openai-responses-proxy/internal/config"
	"openai-responses-proxy/internal/logging"
	"openai-responses-proxy/internal/proxy"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches the command to its handler.
func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "serve", "reload-keys":
		return runService(args)
	case "version":
		return runPrintVersion(args[1:])
	default:
		usage()
		return 2
	}
}

func runService(args []string) int {
	if len(args) != 2 || args[1] == "" {
		usage()
		return 2
	}
	cfg, err := config.Load(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 1
	}
	switch args[0] {
	case "serve":
		if err := serve(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "service error:", err)
			return 1
		}
	case "reload-keys":
		// Use the existing private control socket; this command does not start another proxy instance.
		if err := admin.Reload(cfg.StateDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Fprintln(os.Stdout, "API keys reloaded")
	}
	return 0
}

func runPrintVersion(args []string) int {
	if len(args) != 0 {
		usage()
		return 2
	}
	printVersion()
	return 0
}

// serve starts the proxy dependencies and drains them in order when the service stops.
func serve(cfg config.Config) error {
	// Load users before starting listeners so invalid credentials never leave a partial service running.
	if err := admin.EnsureStateDir(cfg.StateDir); err != nil {
		return err
	}
	configuredUsers, err := config.LoadUsers(cfg.UsersFile)
	if err != nil {
		return err
	}
	logger, err := logging.New(cfg.Logging.Level, cfg.Logging.Destination)
	if err != nil {
		return fmt.Errorf("configure logging: %w", err)
	}
	defer logger.Close()

	store, err := accounting.Open(filepath.Join(cfg.StateDir, "accounting.sqlite"))
	if err != nil {
		return err
	}
	// This guard covers early returns; the normal shutdown path closes the store under its shared deadline.
	storeClosed := false
	defer func() {
		if storeClosed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeouts.Shutdown)
		defer cancel()
		if err := store.CloseContext(ctx); err != nil {
			logger.Error("close accounting store", "error", err.Error())
		}
	}()

	// The reload server and request handlers share this atomic user map.
	activeUsers := auth.NewUsers(configuredUsers)
	control, err := admin.Start(cfg.StateDir, cfg.UsersFile, activeUsers, logger)
	if err != nil {
		return err
	}
	// Retain a bounded fallback for startup failures before the orderly shutdown sequence is reached.
	controlClosed := false
	defer func() {
		if !controlClosed {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeouts.Shutdown)
			defer cancel()
			_ = control.Close(ctx)
		}
	}()

	serviceContext, cancelService := context.WithCancel(context.Background())
	serviceStopped := false
	defer func() {
		if !serviceStopped {
			cancelService()
		}
	}()
	proxyServer, err := proxy.New(cfg, activeUsers, store, logger, serviceContext)
	if err != nil {
		return err
	}
	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ready := proxyServer.StartBackground()
	// Cancel recovery work on early returns; successful shutdown performs the same cleanup in a controlled order.
	defer func() {
		if !serviceStopped {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeouts.Shutdown)
			defer cancel()
			cancelService()
			_ = proxyServer.StopBackground(ctx)
			proxyServer.Close()
		}
	}()
	// Wait for background recovery startup before exposing the request listener.
	select {
	case startupErr := <-ready:
		if startupErr != nil {
			if signals.Err() != nil {
				return nil
			}
			return startupErr
		}
	case <-signals.Done():
		return nil
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	// Bound slow headers and idle keep-alive connections without cutting off long SSE bodies with a write deadline.
	httpServer := &http.Server{
		Handler:           proxyServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- httpServer.Serve(listener)
	}()

	// Either a process signal or an unexpected listener exit enters the same bounded drain path.
	var serveErr error
	select {
	case <-signals.Done():
	case serveErr = <-serveResult:
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), cfg.Timeouts.Shutdown)
	defer cancelShutdown()
	// Stop new work first, then drain handlers and recovery pollers under one shared deadline.
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		_ = httpServer.Close()
		serveErr = errors.Join(serveErr, fmt.Errorf("shut down HTTP listener: %w", err))
	}
	handlerWaitErr := proxyServer.WaitHandlers(shutdownContext)
	if handlerWaitErr != nil {
		_ = httpServer.Close()
		serveErr = errors.Join(serveErr, fmt.Errorf("wait for HTTP handlers: %w", handlerWaitErr))
	}
	if err := control.Close(shutdownContext); err != nil {
		serveErr = errors.Join(serveErr, fmt.Errorf("shut down reload socket: %w", err))
	}
	controlClosed = true
	cancelService()
	backgroundWaitErr := proxyServer.StopBackground(shutdownContext)
	if backgroundWaitErr != nil {
		serveErr = errors.Join(serveErr, fmt.Errorf("wait for background pollers: %w", backgroundWaitErr))
	}
	proxyServer.Close()
	serviceStopped = true
	if handlerWaitErr != nil || backgroundWaitErr != nil || shutdownContext.Err() != nil {
		// Avoid another close wait after the drain budget expires; durable pending rows can resume after restart.
		storeClosed = true
		logger.Error("accounting store close skipped after shutdown deadline; durable pending rows will be reconciled on restart, while unflushed updates may be lost")
	} else {
		if err := store.CloseContext(shutdownContext); err != nil {
			serveErr = errors.Join(serveErr, fmt.Errorf("accounting store could not be closed cleanly: %w", err))
		}
		storeClosed = true
	}
	if serveErr != nil {
		logger.Error("HTTP server stopped unexpectedly", "error", serveErr.Error())
		return serveErr
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: responses-proxy serve CONFIG")
	fmt.Fprintln(os.Stderr, "       responses-proxy reload-keys CONFIG")
	fmt.Fprintln(os.Stderr, "       responses-proxy version")
}
