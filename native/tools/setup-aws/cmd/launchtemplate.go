package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Each node group gets its own launch template with the EC2 settings that
// matter for isolation: IMDSv2 only with a hop limit of 1, so a pod in its
// own network namespace cannot get an IMDS token (and with it the node
// role's credentials); an encrypted gp3 root volume; and, on microvm, nested
// virtualization, which gives a regular instance /dev/kvm for Kata. Without
// a template EKS makes one with hop limit 2.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/launch-templates.html
//	https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-options.html
//	https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html
type ltRef struct {
	id      string
	version int64
}

// launchTemplateName is lib/common.sh's LT_NAME for microvm; the system and
// gvisor groups, whose templates eksctl used to make, follow the same form.
func (r *clusterRun) launchTemplateName(g nodeGroupSpec) string {
	if g.NestedVirt {
		return r.e.names.Cluster + "-microvm-nested-virt"
	}
	return r.e.names.Cluster + "-" + g.Name + "-nodes"
}

// rootDevice is the root volume of the Amazon Linux 2023 EKS AMIs.
const rootDevice = "/dev/xvda"

func (r *clusterRun) launchTemplateData(g nodeGroupSpec) *ec2types.RequestLaunchTemplateData {
	d := &ec2types.RequestLaunchTemplateData{
		MetadataOptions: &ec2types.LaunchTemplateInstanceMetadataOptionsRequest{
			HttpTokens:              ec2types.LaunchTemplateHttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpEndpoint:            ec2types.LaunchTemplateInstanceMetadataEndpointStateEnabled,
		},
		BlockDeviceMappings: []ec2types.LaunchTemplateBlockDeviceMappingRequest{{
			DeviceName: aws.String(rootDevice),
			Ebs: &ec2types.LaunchTemplateEbsBlockDeviceRequest{VolumeSize: aws.Int32(g.VolumeGB), VolumeType: ec2types.VolumeTypeGp3,
				DeleteOnTermination: aws.Bool(true), Encrypted: aws.Bool(true)},
		}},
	}
	if g.NestedVirt {
		d.CpuOptions = &ec2types.LaunchTemplateCpuOptionsRequest{NestedVirtualization: ec2types.NestedVirtualizationSpecificationEnabled}
	}
	// The node group's instances, their volumes and network interfaces get
	// the deployment's tags at launch.
	name := r.e.names.Cluster + "-" + g.Name
	for _, t := range []ec2types.ResourceType{ec2types.ResourceTypeInstance, ec2types.ResourceTypeVolume, ec2types.ResourceTypeNetworkInterface} {
		d.TagSpecifications = append(d.TagSpecifications, ec2types.LaunchTemplateTagSpecificationRequest{
			ResourceType: t, Tags: r.ec2Tags(map[string]string{"Name": name})})
	}
	return d
}

// ltSettings is create-cluster.sh's lt_digest: the settings of a template
// version that matter for isolation, as one comparable value.
type ltSettings struct {
	NestedVirt, Tokens, Endpoint, VolumeType string
	HopLimit, VolumeSize                     int32
	Encrypted, DeleteOnTermination           bool
}

func requestSettings(d *ec2types.RequestLaunchTemplateData) ltSettings {
	s := ltSettings{}
	if d.CpuOptions != nil {
		s.NestedVirt = string(d.CpuOptions.NestedVirtualization)
	}
	if m := d.MetadataOptions; m != nil {
		s.Tokens, s.Endpoint, s.HopLimit = string(m.HttpTokens), string(m.HttpEndpoint), aws.ToInt32(m.HttpPutResponseHopLimit)
	}
	for _, b := range d.BlockDeviceMappings {
		if aws.ToString(b.DeviceName) == rootDevice && b.Ebs != nil {
			s.VolumeType, s.VolumeSize = string(b.Ebs.VolumeType), aws.ToInt32(b.Ebs.VolumeSize)
			s.Encrypted, s.DeleteOnTermination = aws.ToBool(b.Ebs.Encrypted), aws.ToBool(b.Ebs.DeleteOnTermination)
		}
	}
	return s
}

func responseSettings(d *ec2types.ResponseLaunchTemplateData) ltSettings {
	s := ltSettings{}
	if d == nil {
		return s
	}
	if d.CpuOptions != nil {
		s.NestedVirt = string(d.CpuOptions.NestedVirtualization)
	}
	if m := d.MetadataOptions; m != nil {
		s.Tokens, s.Endpoint, s.HopLimit = string(m.HttpTokens), string(m.HttpEndpoint), aws.ToInt32(m.HttpPutResponseHopLimit)
	}
	for _, b := range d.BlockDeviceMappings {
		if aws.ToString(b.DeviceName) == rootDevice && b.Ebs != nil {
			s.VolumeType, s.VolumeSize = string(b.Ebs.VolumeType), aws.ToInt32(b.Ebs.VolumeSize)
			s.Encrypted, s.DeleteOnTermination = aws.ToBool(b.Ebs.Encrypted), aws.ToBool(b.Ebs.DeleteOnTermination)
		}
	}
	return s
}

// ensureLaunchTemplates makes the template of every node group still to
// create. An existing node group keeps the template version it runs: moving
// it to a new version replaces its nodes, which is a decision for a person.
func (r *clusterRun) ensureLaunchTemplates(ctx context.Context) error {
	for _, g := range r.groups() {
		if r.nodegroups[g.Name] != nil {
			slog.Info("node group exists; its launch template is left as it is", "nodegroup", g.Name)
			continue
		}
		ref, err := r.ensureLaunchTemplate(ctx, g)
		if err != nil {
			return fmt.Errorf("launch template %s: %w", r.launchTemplateName(g), err)
		}
		r.templates[g.Name] = ref
	}
	return nil
}

// ensureLaunchTemplate creates the template, or reuses it when it carries
// this deployment's owner tag: as is when its latest version has the wanted
// settings, else with a new version that has them. A template without the
// owner tag is never used or changed.
func (r *clusterRun) ensureLaunchTemplate(ctx context.Context, g nodeGroupSpec) (ltRef, error) {
	name := r.launchTemplateName(g)
	data := r.launchTemplateData(g)
	out, err := r.c.EC2.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{Filters: []ec2types.Filter{filter("launch-template-name", name)}})
	if err != nil {
		return ltRef{}, fmt.Errorf("describe: %w", err)
	}
	if len(out.LaunchTemplates) == 0 {
		ref := ltRef{id: plannedPrefix + name, version: 1}
		err := r.create("launch template "+name, func() error {
			o, err := r.c.EC2.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{LaunchTemplateName: aws.String(name),
				LaunchTemplateData: data, TagSpecifications: r.tagSpec(ec2types.ResourceTypeLaunchTemplate, name)})
			if err != nil {
				return err
			}
			ref = ltRef{id: aws.ToString(o.LaunchTemplate.LaunchTemplateId), version: aws.ToInt64(o.LaunchTemplate.LatestVersionNumber)}
			return nil
		})
		return ref, err
	}
	lt := out.LaunchTemplates[0]
	ref := ltRef{id: aws.ToString(lt.LaunchTemplateId), version: aws.ToInt64(lt.LatestVersionNumber)}
	if owner := ec2TagMap(lt.Tags)[OwnerTagKey]; owner != r.e.names.OwnerValue {
		return ref, fmt.Errorf("%s exists but is not owned by %s (%s=%q); delete or rename it, or pick another CLUSTER_NAME",
			ref.id, r.e.names.OwnerValue, OwnerTagKey, owner)
	}
	v, err := r.c.EC2.DescribeLaunchTemplateVersions(ctx, &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(ref.id), Versions: []string{strconv.FormatInt(ref.version, 10)}})
	if err != nil {
		return ref, fmt.Errorf("describe version %d of %s: %w", ref.version, ref.id, err)
	}
	if len(v.LaunchTemplateVersions) == 1 && responseSettings(v.LaunchTemplateVersions[0].LaunchTemplateData) == requestSettings(data) {
		slog.Info("reusing launch template", "template", name, "id", ref.id, "version", ref.version)
		return ref, nil
	}
	err = r.change(fmt.Sprintf("add a version with the wanted settings to launch template %s (%s)", name, ref.id), func() error {
		o, err := r.c.EC2.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
			LaunchTemplateId: aws.String(ref.id), LaunchTemplateData: data})
		if err != nil {
			return err
		}
		ref.version = aws.ToInt64(o.LaunchTemplateVersion.VersionNumber)
		return nil
	})
	return ref, err
}
