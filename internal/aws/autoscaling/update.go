package autoscaling

import (
	"context"
	"fmt"
	"log"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/doitintl/spot-asg/internal/aws/ec2"
	"github.com/doitintl/spot-asg/internal/aws/sts"

	ec2instancesinfo "github.com/cristim/ec2-instances-info"
)

const (
	spotAllocationStrategy              = "capacity-optimized"
	onDemandBaseCapacity                = 0
	onDemandPercentageAboveBaseCapacity = 0
)

type asgAutoScalingUpdater interface {
	UpdateAutoScalingGroupWithContext(aws.Context, *autoscaling.UpdateAutoScalingGroupInput, ...request.Option) (*autoscaling.UpdateAutoScalingGroupOutput, error)
}

var (
	ec2data *ec2instancesinfo.InstanceData
)

func init() {
	data, err := ec2instancesinfo.Data() // load data only once
	if err != nil {
		log.Fatalln("failed to load binary serialized JSON sourced from ec2instances.info")
	}
	ec2data = data
}

type asgUpdaterService struct {
	asgsvc asgAutoScalingUpdater
	ec2svc ec2.LaunchTemplateVersionDescriber
}

//AsgUpdater ASG Updater interface
type AsgUpdater interface {
	UpdateAutoScalingGroup(ctx context.Context, group *autoscaling.Group) error
}

//NewAsgLister create new ASG Lister
func NewAsgUpdater(role sts.AssumeRoleInRegion) AsgUpdater {
	return &asgUpdaterService{
		asgsvc: autoscaling.New(sts.MustAwsSession(role.Arn, role.ExternalID, role.Region)),
		ec2svc: ec2.NewLaunchTemplateVersionDescriber(role),
	}
}

func (s *asgUpdaterService) UpdateAutoScalingGroup(ctx context.Context, group *autoscaling.Group) error {
	if group == nil {
		return nil
	}
	log.Printf("updating autoscaling group: %v", group.AutoScalingGroupARN)
	// get overrides (types, weights) from asg
	overrides, err := s.getOverrides(ctx, group)
	if err != nil {
		return err
	}
	// prepare request
	mixedInstancePolicy := &autoscaling.MixedInstancesPolicy{
		InstancesDistribution: &autoscaling.InstancesDistribution{
			OnDemandBaseCapacity:                aws.Int64(onDemandBaseCapacity),
			OnDemandPercentageAboveBaseCapacity: aws.Int64(onDemandPercentageAboveBaseCapacity),
			SpotAllocationStrategy:              aws.String(spotAllocationStrategy),
		},
		LaunchTemplate: &autoscaling.LaunchTemplate{
			Overrides: overrides,
		},
	}
	input := &autoscaling.UpdateAutoScalingGroupInput{
		MixedInstancesPolicy: mixedInstancePolicy,
	}
	output, err := s.asgsvc.UpdateAutoScalingGroupWithContext(ctx, input)
	if err != nil {
		return fmt.Errorf("error updading autoscaling group: %v", err)
	}
	log.Printf("updated autoscaling group: %v", output)
	return nil
}

func (s *asgUpdaterService) getOverrides(ctx context.Context, group *autoscaling.Group) ([]*autoscaling.LaunchTemplateOverrides, error) {
	instanceType := ""
	if group.LaunchConfigurationName != nil {
		//instanceType = getInstanceTypeFromLaunchConfiguration()
	} else if group.LaunchTemplate != nil {
		itype, err := s.ec2svc.GetInstanceType(ctx, group.LaunchTemplate)
		if err != nil {
			return nil, fmt.Errorf("error getting instance type from launch template: %v", err)
		}
		instanceType = itype
	}
	if instanceType == "" {
		return nil, fmt.Errorf("failed to detect instance type for autoscaling group: %v", group.AutoScalingGroupARN)
	}
	// iterate over good candidates and add them with weights based on #vCPU
	candidates := getGoodCandidates(instanceType)
	ltOverrides := make([]*autoscaling.LaunchTemplateOverrides, len(candidates))
	for i, c := range candidates {
		ltOverrides[i] = &autoscaling.LaunchTemplateOverrides{
			InstanceType:     aws.String(c.instanceType),
			WeightedCapacity: aws.String(strconv.Itoa(c.weight)),
		}
	}
	return ltOverrides, nil
}

type instanceTypeWeight struct {
	instanceType string // instance type name
	weight       int    // weight by # of vCPU
}

func getGoodCandidates(instanceType string) []instanceTypeWeight {
	var candidates []instanceTypeWeight
	for _, it := range *ec2data {
		if it.InstanceType == instanceType {
			// it - points to instance type in ec2data slice
			// find instances of the same: family, architecture, virtualization type (TBD)
			for _, nt := range *ec2data {
				if it.Arch[0] == nt.Arch[0] &&
					it.Family == nt.Family {
					candidates = append(candidates, instanceTypeWeight{nt.InstanceType, nt.VCPU})
				}
			}
			// no need to continue
			break
		}
	}
	return candidates
}
