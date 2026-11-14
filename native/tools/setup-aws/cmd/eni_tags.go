package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// eniAPI is what tagVPCInterfaces calls. The EC2 clients of create cluster,
// bootstrap, create rds and the plugins all have it.
type eniAPI interface {
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	CreateTags(context.Context, *ec2.CreateTagsInput, ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

// tagVPCInterfaces gives every network interface in vpc that lacks this
// deployment's tags the owner tag and TAGS (not the created-by tag: AWS made
// it, not this tool). AWS services make most of them for what this tool
// creates, and no create call can tag them: EKS's control-plane interfaces
// (on sub-e2e it attached a second one 42 s after create cluster's pass), the
// NAT gateway's, the RDS instance's, the EFS and S3 Files mount targets' and
// the rotation functions' (Lambda). So create cluster, bootstrap, create rds
// and the efs, s3files and db-rotation plugins each run it last.
//
// Only a VPC carrying this deployment's owner tag is touched, and every write
// goes through env.act, so --plan writes nothing. An interface with another
// deployment's owner tag is left as it is. An interface AWS refuses to tag
// is one warning naming it and the reason, never an error: the step has made
// what it makes, and teardown finds the interface's parent by its own tags.
func (e *env) tagVPCInterfaces(ctx context.Context, c eniAPI, vpc string) error {
	if vpc == "" || planned(vpc) {
		return nil
	}
	vpcs, err := c.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []ec2types.Filter{flt("vpc-id", vpc)}})
	if err != nil {
		return fmt.Errorf("describe VPC %s: %w", vpc, err)
	}
	if len(vpcs.Vpcs) != 1 || ec2TagMap(vpcs.Vpcs[0].Tags)[OwnerTagKey] != e.names.OwnerValue {
		slog.Info("the cluster's VPC is not this deployment's; its network interfaces are left as they are", "vpc", vpc)
		return nil
	}
	var enis []ec2types.NetworkInterface
	// IncludeManagedResources: EKS Auto Mode's node interfaces are hidden
	// without it.
	p := ec2.NewDescribeNetworkInterfacesPaginator(c, &ec2.DescribeNetworkInterfacesInput{
		Filters: []ec2types.Filter{flt("vpc-id", vpc)}, IncludeManagedResources: aws.Bool(true)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("describe the network interfaces of VPC %s: %w", vpc, err)
		}
		enis = append(enis, page.NetworkInterfaces...)
	}
	tagged := 0
	var left []string
	for _, n := range enis {
		id, have := aws.ToString(n.NetworkInterfaceId), ec2TagMap(n.TagSet)
		if o := have[OwnerTagKey]; o != "" && o != e.names.OwnerValue {
			slog.Warn("a network interface in the VPC carries another deployment's owner tag; it is left as it is", "interface", id, "owner", o)
			left = append(left, id)
			continue
		}
		var add []ec2types.Tag
		for _, k := range sortedKeys(e.tags) {
			if have[k] != e.tags[k] {
				add = append(add, ec2types.Tag{Key: aws.String(k), Value: aws.String(e.tags[k])})
			}
		}
		if len(add) == 0 {
			continue
		}
		kind := eniKind(n)
		err := e.act(fmt.Sprintf("tag network interface %s (%s)", id, kind), func() error {
			_, err := c.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: add})
			return err
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("AWS refused to tag a network interface; it is left without the owner tag", "interface", id, "kind", kind, "reason", err.Error())
			left = append(left, id)
			continue
		}
		tagged++
	}
	switch {
	case e.cfg.Plan:
	case len(left) > 0:
		slog.Info("network interfaces in the VPC", "vpc", vpc, "interfaces", len(enis), "tagged", tagged, "untagged", strings.Join(left, " "))
	default:
		slog.Info("every network interface in the VPC carries the owner tag", "vpc", vpc, "interfaces", len(enis), "tagged", tagged)
	}
	return nil
}

// eniKind names a network interface by what made it: its description (EC2
// gives the service's, such as "RDSNetworkInterface"), else its type.
func eniKind(n ec2types.NetworkInterface) string {
	if d := aws.ToString(n.Description); d != "" {
		return d
	}
	return string(n.InterfaceType)
}
