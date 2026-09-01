package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
)

func TestServeRejectsInvalidEndpointsBeforeRegistration(t *testing.T) {
	tests := []string{"", "tcp://127.0.0.1:1234", "unix://"}
	for _, endpoint := range tests {
		t.Run(endpoint, func(t *testing.T) {
			called := false
			err := Serve(endpoint, func(*grpc.Server) { called = true })
			if err == nil || !strings.Contains(err.Error(), "absolute unix URL") {
				t.Fatalf("expected endpoint error, got %v", err)
			}
			if called {
				t.Fatal("registration ran for an invalid endpoint")
			}
		})
	}
}

func TestServePreparesUnixSocketAndRunsRegistration(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "shiftpv-server-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "nested", "csi.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	wasSocket := false
	err = Serve("unix://"+socket, func(server *grpc.Server) {
		called = true
		info, statErr := os.Stat(socket)
		wasSocket = statErr == nil && info.Mode()&os.ModeSocket != 0
		server.Stop()
	})
	if !called {
		t.Fatalf("registration was not called: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "server has been stopped") {
		t.Fatalf("expected stopped server result, got %v", err)
	}
	if !wasSocket {
		t.Fatal("registration did not observe a unix socket")
	}
}
