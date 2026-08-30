package platformapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestValidateListenAddressRejectsNonLoopbackAndHostname(t *testing.T) {
	for _, address := range []string{"localhost:7788", "0.0.0.0:7788", ":7788", "192.168.1.20:7788"} {
		if err := ValidateListenAddress(address); err == nil {
			t.Fatalf("ValidateListenAddress(%q) accepted unsafe address", address)
		}
	}
	for _, address := range []string{"127.0.0.1:0", "[::1]:7788"} {
		if err := ValidateListenAddress(address); err != nil {
			t.Fatalf("ValidateListenAddress(%q) error = %v", address, err)
		}
	}
}

func TestServeStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, "127.0.0.1:0", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}), func(address string) error {
			ready <- address
			return nil
		})
	}()
	select {
	case address := <-ready:
		if !strings.HasPrefix(address, "127.0.0.1:") {
			t.Fatalf("ready address = %q", address)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
}
