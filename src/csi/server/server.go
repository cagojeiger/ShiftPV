package server

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
)

type Register func(*grpc.Server)

func Serve(endpoint string, register Register) error {
	return ServeContext(context.Background(), endpoint, register)
}

func ServeContext(ctx context.Context, endpoint string, register Register) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parse CSI endpoint: %w", err)
	}
	if u.Scheme != "unix" || u.Path == "" {
		return fmt.Errorf("CSI endpoint must be an absolute unix URL")
	}
	if err := os.MkdirAll(filepath.Dir(u.Path), 0o750); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	if err := os.Remove(u.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", u.Path)
	if err != nil {
		return fmt.Errorf("listen on CSI socket: %w", err)
	}
	defer listener.Close()

	grpcServer := grpc.NewServer()
	register(grpcServer)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			grpcServer.GracefulStop()
		case <-done:
		}
	}()
	err = grpcServer.Serve(listener)
	close(done)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
