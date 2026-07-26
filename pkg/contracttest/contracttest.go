// Package contracttest provides reusable conformance checks for external
// OpenShell compute drivers.
package contracttest

import (
	"context"
	"testing"

	pb "github.com/rsturla/openshell-drivers-contrib/gen/computepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// RunValidation verifies capability and validation behavior common to all
// drivers. valid must be accepted without creating provider resources.
func RunValidation(t *testing.T, driver pb.ComputeDriverServer, valid *pb.DriverSandbox) {
	t.Helper()
	t.Run("capabilities", func(t *testing.T) {
		response, err := driver.GetCapabilities(context.Background(), &pb.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if response.GetDriverName() == "" || response.GetDriverVersion() == "" {
			t.Fatalf("incomplete capabilities: %v", response)
		}
	})
	t.Run("valid create shape", func(t *testing.T) {
		_, err := driver.ValidateSandboxCreate(context.Background(), &pb.ValidateSandboxCreateRequest{Sandbox: proto.Clone(valid).(*pb.DriverSandbox)})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing sandbox", func(t *testing.T) {
		_, err := driver.ValidateSandboxCreate(context.Background(), &pb.ValidateSandboxCreateRequest{})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("got %v, want InvalidArgument", err)
		}
	})
	t.Run("missing spec", func(t *testing.T) {
		invalid := proto.Clone(valid).(*pb.DriverSandbox)
		invalid.Spec = nil
		_, err := driver.ValidateSandboxCreate(context.Background(), &pb.ValidateSandboxCreateRequest{Sandbox: invalid})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("got %v, want InvalidArgument", err)
		}
	})
	t.Run("control characters rejected", func(t *testing.T) {
		invalid := proto.Clone(valid).(*pb.DriverSandbox)
		invalid.Name += "\nruncmd:"
		_, err := driver.ValidateSandboxCreate(context.Background(), &pb.ValidateSandboxCreateRequest{Sandbox: invalid})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("got %v, want InvalidArgument", err)
		}
	})
	t.Run("invalid resource quantity rejected", func(t *testing.T) {
		invalid := proto.Clone(valid).(*pb.DriverSandbox)
		invalid.Spec.Template.Resources = &pb.DriverResourceRequirements{CpuRequest: "NaN"}
		_, err := driver.ValidateSandboxCreate(context.Background(), &pb.ValidateSandboxCreateRequest{Sandbox: invalid})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("got %v, want InvalidArgument", err)
		}
	})
}
