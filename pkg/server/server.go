package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	pb "github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Initializer interface{ Initialize(context.Context) error }
type WatchStarter interface{ StartWatcher(context.Context) error }

type Options struct {
	SocketPath               string
	BindAddress              string // plaintext development mode; loopback only
	InsecureGatewayTransport bool   // supervisor-to-gateway transport, not this local listener
	ShutdownTimeout          time.Duration
	MaxRecvMsgSize           int
	MaxConcurrentRPCs        int
}

func Run(ctx context.Context, opts Options, drv pb.ComputeDriverServer) error {
	runCtx, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	if opts.MaxRecvMsgSize <= 0 {
		opts.MaxRecvMsgSize = 1 << 20
	}
	if opts.MaxConcurrentRPCs <= 0 {
		opts.MaxConcurrentRPCs = 32
	}
	if initializer, ok := drv.(Initializer); ok {
		initCtx, cancel := context.WithTimeout(runCtx, 2*time.Minute)
		err := initializer.Initialize(initCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("initial driver reconciliation: %w", err)
		}
	}

	lis, cleanup, err := listen(opts)
	if err != nil {
		return err
	}
	defer cleanup()

	sem := make(chan struct{}, opts.MaxConcurrentRPCs)
	unaryLimit := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if opts.InsecureGatewayTransport {
			slog.Warn("DANGER: processing RPC while supervisor-to-gateway transport is plaintext", "rpc", info.FullMethod)
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			return handler(ctx, req)
		default:
			return nil, status.Errorf(codes.ResourceExhausted, "too many concurrent RPCs for %s", info.FullMethod)
		}
	}
	streamWarning := func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if opts.InsecureGatewayTransport {
			slog.Warn("DANGER: processing RPC while supervisor-to-gateway transport is plaintext", "rpc", info.FullMethod)
		}
		return handler(srv, stream)
	}
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(opts.MaxRecvMsgSize),
		grpc.MaxConcurrentStreams(uint32(opts.MaxConcurrentRPCs)),
		grpc.UnaryInterceptor(unaryLimit),
		grpc.StreamInterceptor(streamWarning),
	)
	pb.RegisterComputeDriverServer(grpcServer, drv)

	workerCtx, cancelWorkers := context.WithCancel(runCtx)
	defer cancelWorkers()

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(lis) }()
	watchErr := make(chan error, 1)
	if watcher, ok := drv.(WatchStarter); ok {
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					watchErr <- fmt.Errorf("watcher panic: %v\n%s", recovered, debug.Stack())
				}
			}()
			watchErr <- watcher.StartWatcher(workerCtx)
		}()
	} else {
		watchErr = nil
	}

	slog.Info("gRPC server starting", "address", lis.Addr().String())
	var runErr error
	select {
	case <-runCtx.Done():
		runErr = context.Cause(runCtx)
	case err := <-serveErr:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			runErr = fmt.Errorf("serve gRPC: %w", err)
		} else if runCtx.Err() == nil {
			runErr = errors.New("gRPC server exited unexpectedly")
		}
	case err := <-watchErr:
		if runCtx.Err() == nil {
			if err == nil {
				err = errors.New("watcher exited unexpectedly")
			}
			runErr = err
		}
	}

	cancelWorkers()
	gracefulDone := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(gracefulDone)
	}()
	select {
	case <-gracefulDone:
	case <-time.After(opts.ShutdownTimeout):
		slog.Warn("gRPC graceful shutdown timed out; forcing stop", "timeout", opts.ShutdownTimeout)
		grpcServer.Stop()
		<-gracefulDone
	}
	if errors.Is(runErr, context.Canceled) {
		return nil
	}
	return runErr
}

func listen(opts Options) (net.Listener, func(), error) {
	if opts.BindAddress != "" {
		host, _, err := net.SplitHostPort(opts.BindAddress)
		if err != nil {
			return nil, func() {}, fmt.Errorf("parse TCP bind address %q: %w", opts.BindAddress, err)
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, func() {}, fmt.Errorf("plaintext TCP bind address %q is not loopback; use a Unix socket", opts.BindAddress)
		}
		lis, err := net.Listen("tcp", opts.BindAddress)
		if err != nil {
			return nil, func() {}, fmt.Errorf("listen on TCP address %q: %w", opts.BindAddress, err)
		}
		return lis, func() { _ = lis.Close() }, nil
	}
	if opts.SocketPath == "" {
		return nil, func() {}, errors.New("unix socket path is required")
	}
	if info, err := os.Lstat(opts.SocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, func() {}, fmt.Errorf("refusing to remove non-socket path %q", opts.SocketPath)
		}
		if err := os.Remove(opts.SocketPath); err != nil {
			return nil, func() {}, fmt.Errorf("remove stale Unix socket %q: %w", opts.SocketPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, func() {}, fmt.Errorf("inspect Unix socket %q: %w", opts.SocketPath, err)
	}
	lis, err := net.Listen("unix", opts.SocketPath)
	if err != nil {
		return nil, func() {}, fmt.Errorf("listen on Unix socket %q: %w", opts.SocketPath, err)
	}
	if err := os.Chmod(opts.SocketPath, 0o600); err != nil {
		_ = lis.Close()
		_ = os.Remove(opts.SocketPath)
		return nil, func() {}, fmt.Errorf("set Unix socket permissions on %q: %w", opts.SocketPath, err)
	}
	cleanup := func() {
		_ = lis.Close()
		_ = os.Remove(opts.SocketPath)
	}
	return lis, cleanup, nil
}
