package platformapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

func ValidateListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return fmt.Errorf("listen address must be host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen host must be a literal loopback IP")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("listen port must be a number between 0 and 65535")
	}
	return nil
}

// Serve binds only after validating the literal loopback address. onReady is
// invoked with the resolved address (important for port 0 tests) before Serve
// begins accepting requests.
func Serve(ctx context.Context, address string, handler http.Handler, onReady func(string) error) error {
	if ctx == nil || handler == nil {
		return fmt.Errorf("context and HTTP handler are required")
	}
	if err := ValidateListenAddress(address); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on local API address: %w", err)
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	if onReady != nil {
		if err := onReady(listener.Addr().String()); err != nil {
			_ = listener.Close()
			return err
		}
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			return fmt.Errorf("shut down local API: %w", err)
		}
		err := <-serveResult
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
