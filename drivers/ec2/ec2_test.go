package ec2

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockEC2API struct {
	runFn      func(*awsec2.RunInstancesInput) (*awsec2.RunInstancesOutput, error)
	describeFn func(*awsec2.DescribeInstancesInput) (*awsec2.DescribeInstancesOutput, error)
	lastRun    *awsec2.RunInstancesInput
}

func (m *mockEC2API) RunInstances(_ context.Context, input *awsec2.RunInstancesInput, _ ...func(*awsec2.Options)) (*awsec2.RunInstancesOutput, error) {
	m.lastRun = input
	if m.runFn != nil {
		return m.runFn(input)
	}
	return &awsec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-new"), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending}}}}, nil
}
func (m *mockEC2API) DescribeInstances(_ context.Context, input *awsec2.DescribeInstancesInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeInstancesOutput, error) {
	return m.describeFn(input)
}
func (*mockEC2API) StopInstances(context.Context, *awsec2.StopInstancesInput, ...func(*awsec2.Options)) (*awsec2.StopInstancesOutput, error) {
	return &awsec2.StopInstancesOutput{}, nil
}
func (*mockEC2API) TerminateInstances(context.Context, *awsec2.TerminateInstancesInput, ...func(*awsec2.Options)) (*awsec2.TerminateInstancesOutput, error) {
	return &awsec2.TerminateInstancesOutput{}, nil
}

func TestAWSAdapterLaunchEnforcesSafetyPolicy(t *testing.T) {
	api := &mockEC2API{}
	provider := NewAWSEC2ClientFromAPI(api)
	created := time.Now()
	_, err := provider.Launch(context.Background(), LaunchSpec{SandboxID: "sb", Name: "name", Namespace: "ns", GatewayID: "gw", ImageID: "ami", InstanceType: "c7i.large", SubnetID: "subnet", SecurityGroupID: "sg", ClientToken: "token", DiskSizeGB: 20, CreatedAt: created, MaxLifetime: "8h0m0s"})
	if err != nil {
		t.Fatal(err)
	}
	input := api.lastRun
	if input.InstanceInitiatedShutdownBehavior != ec2types.ShutdownBehaviorTerminate {
		t.Fatal("instance shutdown does not terminate")
	}
	if input.ClientToken == nil || *input.ClientToken != "token" {
		t.Fatal("idempotency token missing")
	}
	if input.MetadataOptions == nil || input.MetadataOptions.HttpEndpoint != ec2types.InstanceMetadataEndpointStateDisabled {
		t.Fatal("IMDS is not disabled")
	}
	ebs := input.BlockDeviceMappings[0].Ebs
	if !aws.ToBool(ebs.Encrypted) || !aws.ToBool(ebs.DeleteOnTermination) {
		t.Fatal("EBS safety policy missing")
	}
}

func TestAWSAdapterRejectsMalformedLaunchResponse(t *testing.T) {
	for name, output := range map[string]*awsec2.RunInstancesOutput{"nil": nil, "empty": {}, "missing ID": {Instances: []ec2types.Instance{{}}}} {
		t.Run(name, func(t *testing.T) {
			api := &mockEC2API{runFn: func(*awsec2.RunInstancesInput) (*awsec2.RunInstancesOutput, error) { return output, nil }}
			_, err := NewAWSEC2ClientFromAPI(api).Launch(context.Background(), LaunchSpec{CreatedAt: time.Now()})
			if err == nil {
				t.Fatal("expected malformed response error")
			}
		})
	}
}

func TestAWSAdapterPaginatesDescribeInstances(t *testing.T) {
	calls := 0
	api := &mockEC2API{describeFn: func(input *awsec2.DescribeInstancesInput) (*awsec2.DescribeInstancesOutput, error) {
		calls++
		if calls == 1 {
			return &awsec2.DescribeInstancesOutput{NextToken: aws.String("page-2"), Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{InstanceId: aws.String("i-1")}}}}}, nil
		}
		if aws.ToString(input.NextToken) != "page-2" {
			return nil, errors.New("missing page token")
		}
		return &awsec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{InstanceId: aws.String("i-2")}}}}}, nil
	}}
	instances, err := NewAWSEC2ClientFromAPI(api).List(context.Background(), InstanceFilter{GatewayID: "gw"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(instances) != 2 {
		t.Fatalf("calls=%d instances=%d", calls, len(instances))
	}
}
