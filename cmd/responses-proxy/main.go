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
	storeClosed := false
	defer func() {
		if !storeClosed {
			_ = store.Close()
		}
	}()

	activeUsers := auth.NewUsers(configuredUsers)
	control, err := admin.Start(cfg.StateDir, cfg.UsersFile, activeUsers, logger)
	if err != nil {
		return err
	}
	controlClosed := false
	defer func() {
		if !controlClosed {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = control.Close(ctx)
		}
	}()

	serviceContext, cancelService := context.WithCancel(context.Background())
	proxyServer, err := proxy.New(cfg, activeUsers, store, logger, serviceContext)
	if err != nil {
		cancelService()
		return err
	}
	serviceStopped := false
	defer func() {
		if !serviceStopped {
			cancelService()
			proxyServer.Close()
		}
	}()
	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
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
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		_ = httpServer.Close()
		serveErr = errors.Join(serveErr, fmt.Errorf("shut down HTTP listener: %w", err))
	}
	handlerWaitErr := proxyServer.WaitHandlers(shutdownContext)
	if handlerWaitErr != nil {
		_ = httpServer.Close()
		serveErr = errors.Join(serveErr, fmt.Errorf("wait for HTTP handlers: %w", handlerWaitErr))
		cancelService()
		proxyServer.Close()
		serviceStopped = true
	}
	if err := control.Close(shutdownContext); err != nil {
		serveErr = errors.Join(serveErr, fmt.Errorf("shut down reload socket: %w", err))
	}
	controlClosed = true
	if !serviceStopped {
		cancelService()
		proxyServer.Close()
		serviceStopped = true
	}
	if handlerWaitErr != nil || shutdownContext.Err() != nil {
		storeClosed = true
		logger.Error("accounting store close skipped because handlers did not drain before the shutdown deadline; a one-shot accounting record may be missing")
	} else {
		closed := make(chan error, 1)
		go func() { closed <- store.Close() }()
		select {
		case err := <-closed:
			if err != nil {
				serveErr = errors.Join(serveErr, fmt.Errorf("accounting store could not be closed cleanly: %w", err))
			}
			storeClosed = true
		case <-shutdownContext.Done():
			storeClosed = true
			logger.Error("accounting store close exceeded the shutdown deadline; a one-shot accounting record may be missing")
		}
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
