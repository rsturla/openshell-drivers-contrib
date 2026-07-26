package server

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type testDriver struct {
	pb.UnimplementedComputeDriverServer
	initialized    atomic.Bool
	watcherStopped chan struct{}
}

func (d *testDriver) Initialize(context.Context) error { d.initialized.Store(true); return nil }
func (d *testDriver) StartWatcher(ctx context.Context) error {
	<-ctx.Done()
	close(d.watcherStopped)
	return nil
}
func (*testDriver) WatchSandboxes(_ *pb.WatchSandboxesRequest, stream pb.ComputeDriver_WatchSandboxesServer) error {
	<-stream.Context().Done()
	return stream.Context().Err()
}

func TestRunUnixSocketPermissionsAndShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil && strings.Contains(err.Error(), "operation not permitted") {
		t.Skip("sandbox disallows listeners")
	}
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	driver := &testDriver{watcherStopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{BindAddress: address, ShutdownTimeout: 100 * time.Millisecond}, driver)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("server exited before creating socket: %v", err)
		default:
		}
		conn, err := net.DialTimeout("tcp", address, 10*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket was not created")
		}
		time.Sleep(time.Millisecond)
	}
	if !driver.initialized.Load() {
		t.Fatal("initial reconciliation did not run")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
	select {
	case <-driver.watcherStopped:
	default:
		t.Fatal("watcher was not canceled")
	}
}

func TestRunForcesShutdownWithOpenWatch(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil && strings.Contains(err.Error(), "operation not permitted") {
		t.Skip("sandbox disallows listeners")
	}
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	driver := &testDriver{watcherStopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{BindAddress: address, ShutdownTimeout: 20 * time.Millisecond}, driver)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("server exited before creating socket: %v", err)
		default:
		}
		conn, err := net.DialTimeout("tcp", address, 10*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket timeout")
		}
		time.Sleep(time.Millisecond)
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	if _, err := pb.NewComputeDriverClient(conn).WatchSandboxes(streamCtx, &pb.WatchSandboxesRequest{}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("open watch deadlocked shutdown")
	}
}

func TestUnixSocketPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "driver.sock")
	_, cleanup, err := listen(Options{SocketPath: path})
	if err != nil && strings.Contains(err.Error(), "operation not permitted") {
		t.Skip("sandbox disallows Unix listeners")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode=%o", info.Mode().Perm())
	}
}

func TestListenRejectsUnsafeTargets(t *testing.T) {
	if _, _, err := listen(Options{BindAddress: "0.0.0.0:1234"}); err == nil {
		t.Fatal("accepted non-loopback plaintext TCP")
	}
	path := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listen(Options{SocketPath: path}); err == nil {
		t.Fatal("removed regular file")
	}
	if contents, _ := os.ReadFile(path); string(contents) != "preserve" {
		t.Fatal("regular file was modified")
	}
}
