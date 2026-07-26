package ec2

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// InstanceProvider is the domain boundary consumed by the driver. AWS SDK
// pagination, request construction, and response normalization stay behind it.
type InstanceProvider interface {
	Launch(context.Context, LaunchSpec) (Instance, error)
	List(context.Context, InstanceFilter) ([]Instance, error)
	Stop(context.Context, string) error
	Terminate(context.Context, []string) error
}

type LaunchSpec struct {
	SandboxID, Name, Namespace, Workspace string
	GatewayID, ImageID, InstanceType      string
	SubnetID, SecurityGroupID, KeyName    string
	UserData, ClientToken                 string
	UseSpot                               bool
	DiskSizeGB                            int32
	CreatedAt                             time.Time
	MaxLifetime                           string
}

type InstanceFilter struct {
	GatewayID string
	SandboxID string
	Name      string
}

type Instance struct {
	ID, SandboxID, Name, Namespace, Workspace string
	State                                     string
	CreatedAt                                 time.Time
}

// EC2API is the minimal AWS SDK seam used only by the AWS adapter.
type EC2API interface {
	RunInstances(context.Context, *awsec2.RunInstancesInput, ...func(*awsec2.Options)) (*awsec2.RunInstancesOutput, error)
	TerminateInstances(context.Context, *awsec2.TerminateInstancesInput, ...func(*awsec2.Options)) (*awsec2.TerminateInstancesOutput, error)
	StopInstances(context.Context, *awsec2.StopInstancesInput, ...func(*awsec2.Options)) (*awsec2.StopInstancesOutput, error)
	DescribeInstances(context.Context, *awsec2.DescribeInstancesInput, ...func(*awsec2.Options)) (*awsec2.DescribeInstancesOutput, error)
}

type AWSEC2Client struct{ client EC2API }

func NewAWSEC2Client(cfg aws.Config) *AWSEC2Client {
	return &AWSEC2Client{client: awsec2.NewFromConfig(cfg)}
}

// NewAWSEC2ClientFromAPI is intended for adapter tests.
func NewAWSEC2ClientFromAPI(client EC2API) *AWSEC2Client { return &AWSEC2Client{client: client} }

func (c *AWSEC2Client) Launch(ctx context.Context, spec LaunchSpec) (Instance, error) {
	tags := []ec2types.Tag{
		{Key: aws.String(TagSandboxID), Value: aws.String(spec.SandboxID)},
		{Key: aws.String(TagSandboxName), Value: aws.String(spec.Name)},
		{Key: aws.String(TagNamespace), Value: aws.String(spec.Namespace)},
		{Key: aws.String(TagGatewayID), Value: aws.String(spec.GatewayID)},
		{Key: aws.String(TagWorkspace), Value: aws.String(spec.Workspace)},
		{Key: aws.String(TagCreatedAt), Value: aws.String(spec.CreatedAt.UTC().Format(time.RFC3339Nano))},
		{Key: aws.String(TagMaxLifetime), Value: aws.String(spec.MaxLifetime)},
		{Key: aws.String("Name"), Value: aws.String("openshell-" + spec.Name)},
	}
	input := &awsec2.RunInstancesInput{
		ImageId: aws.String(spec.ImageID), InstanceType: ec2types.InstanceType(spec.InstanceType),
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1), SubnetId: aws.String(spec.SubnetID),
		SecurityGroupIds: []string{spec.SecurityGroupID}, UserData: aws.String(spec.UserData),
		ClientToken:                       aws.String(spec.ClientToken),
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate,
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
			{ResourceType: ec2types.ResourceTypeVolume, Tags: tags},
			{ResourceType: ec2types.ResourceTypeNetworkInterface, Tags: tags},
		},
		MetadataOptions: &ec2types.InstanceMetadataOptionsRequest{HttpEndpoint: ec2types.InstanceMetadataEndpointStateDisabled},
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"),
			Ebs:        &ec2types.EbsBlockDevice{VolumeSize: aws.Int32(spec.DiskSizeGB), VolumeType: ec2types.VolumeTypeGp3, Encrypted: aws.Bool(true), DeleteOnTermination: aws.Bool(true)},
		}},
	}
	if spec.KeyName != "" {
		input.KeyName = aws.String(spec.KeyName)
	}
	if spec.UseSpot {
		input.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{MarketType: ec2types.MarketTypeSpot}
		input.TagSpecifications = append(input.TagSpecifications, ec2types.TagSpecification{ResourceType: ec2types.ResourceTypeSpotInstancesRequest, Tags: tags})
	}
	result, err := c.client.RunInstances(ctx, input)
	if err != nil {
		return Instance{}, fmt.Errorf("run EC2 instance: %w", err)
	}
	if result == nil || len(result.Instances) != 1 {
		return Instance{}, fmt.Errorf("run EC2 instance: expected one instance, received malformed response")
	}
	instance := normalizeInstance(result.Instances[0])
	if instance.ID == "" {
		return Instance{}, fmt.Errorf("run EC2 instance: response has empty instance ID")
	}
	if instance.SandboxID == "" {
		instance.SandboxID, instance.Name, instance.Namespace, instance.Workspace = spec.SandboxID, spec.Name, spec.Namespace, spec.Workspace
	}
	if instance.CreatedAt.IsZero() {
		instance.CreatedAt = spec.CreatedAt
	}
	return instance, nil
}

func (c *AWSEC2Client) List(ctx context.Context, filter InstanceFilter) ([]Instance, error) {
	var filters []ec2types.Filter
	if filter.GatewayID != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("tag:" + TagGatewayID), Values: []string{filter.GatewayID}})
	}
	if filter.SandboxID != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("tag:" + TagSandboxID), Values: []string{filter.SandboxID}})
	}
	if filter.Name != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("tag:" + TagSandboxName), Values: []string{filter.Name}})
	}
	paginator := awsec2.NewDescribeInstancesPaginator(c.client, &awsec2.DescribeInstancesInput{Filters: filters})
	var instances []Instance
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("describe EC2 instances: %w", err)
		}
		for _, reservation := range page.Reservations {
			for _, raw := range reservation.Instances {
				instances = append(instances, normalizeInstance(raw))
			}
		}
	}
	return instances, nil
}

func (c *AWSEC2Client) Stop(ctx context.Context, id string) error {
	_, err := c.client.StopInstances(ctx, &awsec2.StopInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return fmt.Errorf("stop EC2 instance %s: %w", id, err)
	}
	return nil
}

func (c *AWSEC2Client) Terminate(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := c.client.TerminateInstances(ctx, &awsec2.TerminateInstancesInput{InstanceIds: ids})
	if err != nil {
		return fmt.Errorf("terminate EC2 instances: %w", err)
	}
	return nil
}

func normalizeInstance(raw ec2types.Instance) Instance {
	tags := make(map[string]string, len(raw.Tags))
	for _, tag := range raw.Tags {
		tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	createdAt := aws.ToTime(raw.LaunchTime)
	if createdAt.IsZero() {
		createdAt, _ = time.Parse(time.RFC3339Nano, tags[TagCreatedAt])
	}
	state := ""
	if raw.State != nil {
		state = string(raw.State.Name)
	}
	return Instance{
		ID: aws.ToString(raw.InstanceId), SandboxID: tags[TagSandboxID], Name: tags[TagSandboxName],
		Namespace: tags[TagNamespace], Workspace: tags[TagWorkspace], State: state, CreatedAt: createdAt,
	}
}
