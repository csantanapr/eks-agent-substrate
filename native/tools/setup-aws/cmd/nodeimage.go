package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
)

// A custom AMI (NODE_AMI_ID) replaces the Amazon EKS optimized Amazon Linux
// 2023 AMI that EKS picks for a managed node group by its AMI type. The AMI
// must join the cluster with nodeadm, as AMIs built from the EKS optimized
// AL2023 AMI or with awslabs/amazon-eks-ami do: the launch template gives
// it a NodeConfig (nodeUserData). It is the customer's: no owner tag, so
// teardown never deletes it.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/launch-templates.html#launch-template-custom-ami
//	https://docs.aws.amazon.com/eks/latest/userguide/eks-optimized-ami.html
type nodeImage struct {
	ID         string
	Name       string
	Arch       ec2types.ArchitectureValues
	RootDevice string
	// RootGB is the size of the root volume's snapshot, the smallest root
	// volume the AMI launches with.
	RootGB int32
	// ENA reports that the AMI has ENA support, which current instance
	// types (the m8i default) require.
	ENA bool
}

// amiIDRE is an EC2 image ID.
var amiIDRE = regexp.MustCompile(`^ami-([0-9a-f]{8}|[0-9a-f]{17})$`)

// archOf is the image architecture of an EKS AMI type.
func archOf(t ekstypes.AMITypes) ec2types.ArchitectureValues {
	if t == ekstypes.AMITypesAl2023Arm64Standard {
		return ec2types.ArchitectureValuesArm64
	}
	return ec2types.ArchitectureValuesX8664
}

// describeImage reads one AMI. An AMI that does not exist, or that this
// account may not launch, is an error with the way out; any other failed
// read stops the run too (it is never taken as "no such AMI").
func describeImage(ctx context.Context, c clusterEC2API, id, setting string) (*nodeImage, error) {
	if !amiIDRE.MatchString(id) {
		return nil, fmt.Errorf("%s=%s is not an AMI ID (ami- and 8 or 17 hexadecimal digits)", setting, id)
	}
	out, err := c.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{id}})
	switch {
	case hasCode(err, "InvalidAMIID.NotFound"), hasCode(err, "InvalidAMIID.Unavailable"), err == nil && len(out.Images) == 0:
		return nil, fmt.Errorf("%s=%s: no such AMI in this Region, or this account may not launch it; copy it to this Region or share it with this account", setting, id)
	case hasCode(err, "InvalidAMIID.Malformed"):
		return nil, fmt.Errorf("%s=%s is not an AMI ID", setting, id)
	case err != nil:
		return nil, fmt.Errorf("describe AMI %s (%s): %w", id, setting, err)
	}
	im := out.Images[0]
	if im.State != ec2types.ImageStateAvailable {
		return nil, fmt.Errorf("%s=%s is %s, not available", setting, id, im.State)
	}
	if im.Platform == ec2types.PlatformValuesWindows {
		return nil, fmt.Errorf("%s=%s is a Windows AMI; a node AMI here joins with nodeadm (Amazon Linux 2023)", setting, id)
	}
	if im.VirtualizationType != ec2types.VirtualizationTypeHvm {
		return nil, fmt.Errorf("%s=%s is a %s AMI; node instance types run HVM AMIs only", setting, id, im.VirtualizationType)
	}
	n := &nodeImage{ID: id, Name: aws.ToString(im.Name), Arch: im.Architecture, RootDevice: aws.ToString(im.RootDeviceName),
		ENA: aws.ToBool(im.EnaSupport)}
	if n.RootDevice == "" {
		return nil, fmt.Errorf("%s=%s has no root device", setting, id)
	}
	ebs := false
	for _, b := range im.BlockDeviceMappings {
		if aws.ToString(b.DeviceName) == n.RootDevice && b.Ebs != nil {
			ebs, n.RootGB = true, aws.ToInt32(b.Ebs.VolumeSize)
		}
	}
	if !ebs || im.RootDeviceType != ec2types.DeviceTypeEbs {
		return nil, fmt.Errorf("%s=%s has no EBS root volume; a node AMI here is EBS-backed", setting, id)
	}
	return n, nil
}

// checkImage reads the AMI of the groups to make, when their templates
// name one: NODE_AMI_ID when it is set, else for self-managed groups the
// recommended EKS optimized AL2023 AMI (resolveImages); and for the nitro
// group always NITRO_AMI_ID, which no other group uses. Each AMI's
// architecture must be the one its group's instance type runs, and its root
// snapshot must fit the group's root volume. checkInstanceTypes runs first.
func (r *clusterRun) checkImage(ctx context.Context) error {
	var others []nodeGroupSpec
	var nitro *nodeGroupSpec
	for _, g := range r.groups() {
		if g.Name == nodegroups.Nitro {
			nitro = &g
			continue
		}
		others = append(others, g)
	}
	named := len(others) > 0 && (r.cfg.NodeAMIID != "" || r.selfManaged())
	if !named && nitro == nil {
		return nil
	}
	// The NodeConfig carries an IPv4 service CIDR; a new cluster is IPv4.
	if r.cluster != nil && r.cluster.KubernetesNetworkConfig != nil && r.cluster.KubernetesNetworkConfig.IpFamily == ekstypes.IpFamilyIpv6 {
		return fmt.Errorf("cluster %s is an IPv6 cluster; nodes that join from a NodeConfig (NODE_AMI_ID, NODE_GROUP_TYPE=self-managed, the nitro node group) support IPv4 clusters only", r.e.names.Cluster)
	}
	switch {
	case !named:
	case r.cfg.NodeAMIID == "":
		if err := r.resolveImages(ctx); err != nil {
			return err
		}
	default:
		im, err := describeImage(ctx, r.c.EC2, r.cfg.NodeAMIID, "NODE_AMI_ID")
		if err != nil {
			return err
		}
		for _, g := range others {
			if err := r.imageFits(im, "NODE_AMI_ID", g); err != nil {
				return err
			}
		}
		r.image = im
		slog.Info("custom node AMI", "ami", im.ID, "name", im.Name, "arch", string(im.Arch), "rootDevice", im.RootDevice)
	}
	if nitro == nil {
		return nil
	}
	im, err := describeImage(ctx, r.c.EC2, r.cfg.NitroAMIID, "NITRO_AMI_ID")
	if err != nil {
		return err
	}
	if err := r.imageFits(im, "NITRO_AMI_ID", *nitro); err != nil {
		return err
	}
	r.nitroImage = im
	slog.Info("Nitro Sandbox AMI", "ami", im.ID, "name", im.Name, "arch", string(im.Arch), "rootDevice", im.RootDevice)
	return nil
}

// imageFits refuses im (from setting) for group g.
func (r *clusterRun) imageFits(im *nodeImage, setting string, g nodeGroupSpec) error {
	if r.enaRequired[g.InstanceType] && !im.ENA {
		return fmt.Errorf("%s=%s has no ENA support, which the %s's %s requires", setting, im.ID, g.noun(), g.InstanceType)
	}
	if want := archOf(r.amiTypes[g.InstanceType]); im.Arch != want {
		return fmt.Errorf("%s=%s is %s, but the %s's %s runs %s; pick an AMI or an instance type of the same architecture",
			setting, im.ID, im.Arch, g.noun(), g.InstanceType, want)
	}
	if im.RootGB > g.VolumeGB {
		return fmt.Errorf("%s=%s has a %d GB root snapshot, more than the %s's %d GB root volume", setting, im.ID, im.RootGB, g.noun(), g.VolumeGB)
	}
	return nil
}

// eksAMIParameter is the public SSM parameter that names the recommended
// Amazon EKS optimized AL2023 AMI of a Kubernetes version and architecture.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/retrieve-ami-id.html
func eksAMIParameter(version string, arch ec2types.ArchitectureValues) string {
	return "/aws/service/eks/optimized-ami/" + version + "/amazon-linux-2023/" + string(arch) + "/standard/recommended/image_id"
}

// resolveImages reads, for the self-managed groups still to make, the
// recommended AMI of each architecture they run, for the cluster's version
// (KUBERNETES_VERSION for a new cluster): one SSM read per architecture,
// before any template is made, so every template of the run names the same
// AMI. The parameter changes with each AMI release; a group that exists
// keeps the AMI it was made with.
func (r *clusterRun) resolveImages(ctx context.Context) error {
	version := r.cfg.KubernetesVersion
	if r.cluster != nil && aws.ToString(r.cluster.Version) != "" {
		version = aws.ToString(r.cluster.Version)
	}
	for _, g := range r.groups() {
		// The nitro group runs NITRO_AMI_ID, never the EKS optimized AMI.
		if r.groupExists(g.Name) || g.Name == nodegroups.Nitro {
			continue
		}
		im, name, err := r.recommendedImage(ctx, version, archOf(r.amiTypes[g.InstanceType]))
		if err != nil {
			return err
		}
		if err := r.imageFits(im, "SSM parameter "+name, g); err != nil {
			return err
		}
	}
	return nil
}

// recommendedImage is the recommended EKS optimized AL2023 AMI of
// Kubernetes version and arch, and the SSM parameter that names it: read
// once per architecture in a run (archImages).
func (r *clusterRun) recommendedImage(ctx context.Context, version string, arch ec2types.ArchitectureValues) (*nodeImage, string, error) {
	name := eksAMIParameter(version, arch)
	if im := r.archImages[arch]; im != nil {
		return im, name, nil
	}
	out, err := r.c.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	switch {
	case hasCode(err, "ParameterNotFound"):
		return nil, name, fmt.Errorf("SSM has no parameter %s: no EKS optimized AL2023 AMI for Kubernetes %s on %s in %s; set NODE_AMI_ID", name, version, arch, r.e.names.Region)
	case err != nil:
		return nil, name, fmt.Errorf("read SSM parameter %s: %w", name, err)
	case out.Parameter == nil || aws.ToString(out.Parameter.Value) == "":
		return nil, name, fmt.Errorf("SSM parameter %s is empty", name)
	}
	im, err := describeImage(ctx, r.c.EC2, aws.ToString(out.Parameter.Value), "SSM parameter "+name)
	if err != nil {
		return nil, name, err
	}
	r.archImages[arch] = im
	slog.Info("EKS optimized AMI", "arch", string(arch), "ami", im.ID, "name", im.Name)
	return im, name, nil
}
