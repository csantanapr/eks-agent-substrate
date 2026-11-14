package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// createClusterInput is the EKS cluster create-cluster.sh's eksctl config
// describes, with two deliberate differences: the cluster gets only the
// private subnets (see vpc.go), and the caller's admin access is an explicit
// access entry (ensureAccess) instead of EKS's creator bootstrap, so a rerun
// can check and repair it.
func (r *clusterRun) createClusterInput() *eks.CreateClusterInput {
	in := &eks.CreateClusterInput{
		Name:    aws.String(r.e.names.Cluster),
		Version: aws.String(r.cfg.KubernetesVersion),
		RoleArn: aws.String(r.roleARN(r.clusterRoleName())),
		// The API endpoint is public, limited to EKS_PUBLIC_ACCESS_CIDRS,
		// and private inside the VPC, so nodes reach it without the NAT.
		//   https://docs.aws.amazon.com/eks/latest/userguide/cluster-endpoint.html
		ResourcesVpcConfig: &ekstypes.VpcConfigRequest{
			SubnetIds:             r.private,
			EndpointPublicAccess:  aws.Bool(true),
			EndpointPrivateAccess: aws.Bool(true),
			PublicAccessCidrs:     strings.Fields(r.cfg.PublicAccessCIDRs),
		},
		// Access entries only (no aws-auth ConfigMap).
		//   https://docs.aws.amazon.com/eks/latest/userguide/access-entries.html
		AccessConfig: &ekstypes.CreateAccessConfigRequest{
			AuthenticationMode:                      ekstypes.AuthenticationModeApi,
			BootstrapClusterCreatorAdminPermissions: aws.Bool(false),
		},
		// The add-ons phase installs vpc-cni, kube-proxy and coredns as EKS
		// add-ons; Auto Mode requires this off.
		BootstrapSelfManagedAddons: aws.Bool(false),
		Tags:                       r.tags,
	}
	if r.auto {
		// Auto Mode turns on compute, load balancing and block storage
		// together, with its two built-in NodePools.
		//   https://docs.aws.amazon.com/eks/latest/userguide/set-builtin-node-pools.html
		in.ComputeConfig = &ekstypes.ComputeConfigRequest{Enabled: aws.Bool(true),
			NodePools: []string{"general-purpose", "system"}, NodeRoleArn: aws.String(r.roleARN(r.autoNodeRoleName()))}
		in.KubernetesNetworkConfig = &ekstypes.KubernetesNetworkConfigRequest{ElasticLoadBalancing: &ekstypes.ElasticLoadBalancing{Enabled: aws.Bool(true)}}
		in.StorageConfig = &ekstypes.StorageConfigRequest{BlockStorage: &ekstypes.BlockStorage{Enabled: aws.Bool(true)}}
	}
	return in
}

func (r *clusterRun) ensureCluster(ctx context.Context) error {
	name := r.e.names.Cluster
	if r.cluster == nil {
		in := r.createClusterInput()
		if err := r.create(fmt.Sprintf("EKS cluster %s (Kubernetes %s, Auto Mode %s)", name, r.cfg.KubernetesVersion, onOff(r.auto)), func() error {
			return retryNewRole(ctx, func() error {
				out, err := r.c.EKS.CreateCluster(ctx, in)
				if err == nil {
					r.cluster = out.Cluster
				}
				return err
			})
		}); err != nil {
			return fmt.Errorf("create cluster %s: %w", name, err)
		}
		if r.plan() {
			return nil
		}
	}
	if r.cluster.Status != ekstypes.ClusterStatusActive && r.plan() {
		// A plan reads and reports; it never waits on someone else's
		// create or update to finish (review F17).
		slog.Info("plan: cluster " + name + " is " + string(r.cluster.Status) + "; a real run would wait for ACTIVE")
		return nil
	}
	if r.cluster.Status != ekstypes.ClusterStatusActive {
		if err := waitFor(ctx, "cluster "+name+" to be ACTIVE (about 10 minutes)", 30*time.Minute, func() (bool, error) {
			out, err := r.c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(name)})
			if err != nil {
				return false, fmt.Errorf("describe cluster %s: %w", name, err)
			}
			r.cluster = out.Cluster
			switch out.Cluster.Status {
			case ekstypes.ClusterStatusActive:
				return true, nil
			case ekstypes.ClusterStatusCreating, ekstypes.ClusterStatusUpdating:
				return false, nil
			}
			return false, fmt.Errorf("cluster %s is %s", name, out.Cluster.Status)
		}); err != nil {
			return err
		}
	}
	allowed, err := r.ensurePublicAccess(ctx)
	if err != nil {
		return err
	}
	// Only now is the cluster ACTIVE with any access update Successful.
	if !r.plan() {
		if err := probeCluster(ctx, r.cluster); err != nil {
			return fmt.Errorf("this machine cannot reach the Kubernetes API of cluster %s at %s, which accepts %s: %w. This network may reach AWS from another address than %s sees; %s, then run create cluster again, which updates the ranges in a few minutes",
				name, aws.ToString(r.cluster.Endpoint), strings.Join(allowed, " "), err, checkIPURL, cidrsFix)
		}
		slog.Info("this machine reaches the Kubernetes API", "cluster", name, "connections", probeConns)
	}
	vc := r.cluster.ResourcesVpcConfig
	// EKS makes the cluster security group, which every node uses, and
	// cannot tag it at create time. Only a cluster this tool created gets
	// the created-by tag on it; eksctl's keeps its own tags.
	if vc != nil && aws.ToString(vc.ClusterSecurityGroupId) != "" && r.cluster.Tags[CreatedByTagKey] == CreatedByTagValue {
		id := aws.ToString(vc.ClusterSecurityGroupId)
		out, err := r.c.EC2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{id}})
		if err != nil {
			return fmt.Errorf("describe the cluster security group %s: %w", id, err)
		}
		for _, g := range out.SecurityGroups {
			if err := r.addMissingTags(ctx, "cluster security group", id, g.Tags); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// ensurePublicAccess makes the cluster's public endpoint accept an explicit
// EKS_PUBLIC_ACCESS_CIDRS, exactly that list, so rerunning `create cluster`
// with it is how to change the ranges (or drop an old address). auto never
// changes them: a rerun must not narrow a running cluster by surprise, so it
// logs the ranges the cluster has and leaves them. The endpoint's public and
// private switches are sent as they are. It returns the ranges in effect.
func (r *clusterRun) ensurePublicAccess(ctx context.Context) ([]string, error) {
	vc := r.cluster.ResourcesVpcConfig
	if vc == nil || !vc.EndpointPublicAccess {
		return nil, nil // no public endpoint, so no ranges to keep
	}
	have := vc.PublicAccessCidrs
	if r.autoCIDRs {
		slog.Info("EKS_PUBLIC_ACCESS_CIDRS=auto keeps the cluster's API access ranges; set it to change them", "ranges", strings.Join(have, " "))
		if slices.Contains(have, "0.0.0.0/0") {
			slog.Warn("the Kubernetes API accepts connections from any address (authenticated by IAM); set EKS_PUBLIC_ACCESS_CIDRS to narrow it")
		}
		return have, nil
	}
	want := strings.Fields(r.cfg.PublicAccessCIDRs)
	if slices.Equal(sortedCopy(have), sortedCopy(want)) {
		return have, nil
	}
	name := r.e.names.Cluster
	var id string
	what := fmt.Sprintf("let %s reach the Kubernetes API of cluster %s (it allows %s)", strings.Join(want, " "), name, strings.Join(have, " "))
	if err := r.change(what, func() error {
		out, err := r.c.EKS.UpdateClusterConfig(ctx, &eks.UpdateClusterConfigInput{Name: aws.String(name),
			ResourcesVpcConfig: &ekstypes.VpcConfigRequest{PublicAccessCidrs: want,
				EndpointPublicAccess: aws.Bool(vc.EndpointPublicAccess), EndpointPrivateAccess: aws.Bool(vc.EndpointPrivateAccess)}})
		if err == nil && out.Update != nil {
			id = aws.ToString(out.Update.Id)
		}
		return err
	}); err != nil {
		return nil, fmt.Errorf("update the API access ranges of cluster %s: %w", name, err)
	}
	if r.plan() {
		return have, nil
	}
	return want, waitFor(ctx, "the API access update of cluster "+name+" (a few minutes)", 20*time.Minute, func() (bool, error) {
		out, err := r.c.EKS.DescribeUpdate(ctx, &eks.DescribeUpdateInput{Name: aws.String(name), UpdateId: aws.String(id)})
		if err != nil {
			return false, fmt.Errorf("describe update %s of cluster %s: %w", id, name, err)
		}
		switch out.Update.Status {
		case ekstypes.UpdateStatusSuccessful:
			return true, nil
		case ekstypes.UpdateStatusInProgress:
			return false, nil
		}
		var why []string
		for _, e := range out.Update.Errors {
			why = append(why, string(e.ErrorCode)+": "+aws.ToString(e.ErrorMessage))
		}
		return false, fmt.Errorf("the API access update of cluster %s is %s: %s", name, out.Update.Status, strings.Join(why, "; "))
	})
}

// Replaced in tests. probeConns new connections in a row must reach the
// API: behind a NAT that picks its public address per connection, some of
// them would fail.
var (
	probeConns   = 6
	probeTimeout = 5 * time.Second
	probeWarmup  = 2 * time.Minute
	probeCluster = func(ctx context.Context, c *ekstypes.Cluster) error {
		ca := ""
		if c.CertificateAuthority != nil {
			ca = aws.ToString(c.CertificateAuthority.Data)
		}
		return probeAPI(ctx, aws.ToString(c.Endpoint), ca)
	}
)

// probeAPI checks that this machine reaches the Kubernetes API at endpoint
// the way kubectl does: through HTTPS_PROXY if set, verifying the cluster
// CA. It waits up to probeWarmup for a first answer, while the endpoint's
// name and access rules settle, then needs probeConns-1 more in a row, each
// on a new connection. Any HTTP answer counts (401 included): the request
// carries no credentials, and only reaching the server matters.
func probeAPI(ctx context.Context, endpoint, caData string) error {
	pem, err := base64.StdEncoding.DecodeString(caData)
	pool := x509.NewCertPool()
	if endpoint == "" || err != nil || !pool.AppendCertsFromPEM(pem) {
		return errors.New("the cluster reports no endpoint or certificate authority")
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives, t.TLSClientConfig = true, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	client := &http.Client{Transport: t, Timeout: probeTimeout}
	get := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/version", nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return resp.Body.Close()
	}
	var last error
	if err := waitFor(ctx, "the Kubernetes API to answer this machine", probeWarmup, func() (bool, error) {
		last = get()
		return last == nil, nil
	}); err != nil {
		if ctx.Err() == nil && last != nil {
			return fmt.Errorf("no answer in %s: %w", probeWarmup, last)
		}
		return err
	}
	for i := 2; i <= probeConns; i++ {
		if err := get(); err != nil {
			return fmt.Errorf("connection %d of %d failed after the first answered: %w", i, probeConns, err)
		}
	}
	return nil
}

// clusterAdminPolicy is the EKS access policy the caller gets.
//
//	https://docs.aws.amazon.com/eks/latest/userguide/access-policy-permissions.html
func (r *clusterRun) clusterAdminPolicy() string {
	return fmt.Sprintf("arn:%s:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy", r.e.names.Partition)
}

// callerPrincipal is the IAM principal of the caller's credentials. An
// assumed-role session maps to its role, whose ARN (with any path, which
// access entries accept) comes from IAM.
func (r *clusterRun) callerPrincipal(ctx context.Context) (string, error) {
	id, err := r.e.c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("AWS credentials: %w", err)
	}
	arn := aws.ToString(id.Arn)
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 {
		return "", fmt.Errorf("unexpected caller ARN %q", r.mask(arn))
	}
	res := parts[5]
	switch {
	case parts[2] == "iam" && (strings.HasPrefix(res, "user/") || strings.HasPrefix(res, "role/")):
		return arn, nil
	case parts[2] == "sts" && strings.HasPrefix(res, "assumed-role/"):
		role, _, _ := strings.Cut(strings.TrimPrefix(res, "assumed-role/"), "/")
		out, err := r.c.IAM.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(role)})
		if err != nil {
			return "", fmt.Errorf("get the caller's role %s: %w", role, err)
		}
		return aws.ToString(out.Role.Arn), nil
	}
	return "", fmt.Errorf("the caller %s is not an IAM user or role, so it cannot get an access entry", r.mask(arn))
}

// mask hides the account id in what is logged (last four digits kept).
func (r *clusterRun) mask(s string) string {
	a := r.e.names.Account
	if len(a) < 4 {
		return s
	}
	return strings.ReplaceAll(s, a, "********"+a[len(a)-4:])
}

// ensureAccess gives the caller an access entry with AmazonEKSClusterAdminPolicy
// for the whole cluster, so the installer and kubectl can reach it.
func (r *clusterRun) ensureAccess(ctx context.Context) error {
	principal, err := r.callerPrincipal(ctx)
	if err != nil {
		return err
	}
	who := r.mask(principal)
	cluster := aws.String(r.e.names.Cluster)
	entry := true
	if r.cluster != nil {
		_, err := r.c.EKS.DescribeAccessEntry(ctx, &eks.DescribeAccessEntryInput{ClusterName: cluster, PrincipalArn: aws.String(principal)})
		switch {
		case isNotFound(err):
			entry = false
		case err != nil:
			return fmt.Errorf("describe the access entry of %s: %w", who, err)
		}
	} else {
		entry = false
	}
	if !entry {
		if err := r.create("access entry for "+who, func() error {
			_, err := r.c.EKS.CreateAccessEntry(ctx, &eks.CreateAccessEntryInput{ClusterName: cluster,
				PrincipalArn: aws.String(principal), Type: aws.String("STANDARD"), Tags: r.tags})
			return err
		}); err != nil {
			return fmt.Errorf("access entry of %s: %w", who, err)
		}
	}
	if entry {
		out, err := r.c.EKS.ListAssociatedAccessPolicies(ctx, &eks.ListAssociatedAccessPoliciesInput{ClusterName: cluster, PrincipalArn: aws.String(principal)})
		if err != nil {
			return fmt.Errorf("list the access policies of %s: %w", who, err)
		}
		for _, p := range out.AssociatedAccessPolicies {
			if aws.ToString(p.PolicyArn) == r.clusterAdminPolicy() && p.AccessScope != nil && p.AccessScope.Type == ekstypes.AccessScopeTypeCluster {
				slog.Info("cluster access ok", "principal", who)
				return nil
			}
		}
	}
	do := func() error {
		_, err := r.c.EKS.AssociateAccessPolicy(ctx, &eks.AssociateAccessPolicyInput{ClusterName: cluster,
			PrincipalArn: aws.String(principal), PolicyArn: aws.String(r.clusterAdminPolicy()),
			AccessScope: &ekstypes.AccessScope{Type: ekstypes.AccessScopeTypeCluster}})
		return err
	}
	what := "associate AmazonEKSClusterAdminPolicy with " + who
	if entry {
		err = r.change(what, do)
	} else {
		err = r.e.act(what, do)
	}
	if err != nil {
		return fmt.Errorf("access policy of %s: %w", who, err)
	}
	return nil
}
