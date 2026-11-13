// Package awsx is the wizard's read-only view of AWS, the counterpart of
// substrate-gke's installer/internal/gcp. It calls the AWS SDK for Go
// directly; substrate-gke's gcp client shells out to gcloud for the same
// reads. The provisioning itself is setup-aws, run as its own process.
package awsx

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// MinMinor is the oldest EKS minor version Substrate runs on (1.37: the
// PodCertificate APIs), the floor substrate-gke applies to GKE too.
const MinMinor = 37

// Cluster is one row of the cluster picker.
type Cluster struct {
	Name, Version, Status string
	AutoMode              bool
	Endpoint, CA          string
}

// Supported reports whether the cluster can run Substrate.
func (c Cluster) Supported() bool {
	if c.Status != string(ekstypes.ClusterStatusActive) {
		return false
	}
	_, minor, ok := strings.Cut(c.Version, ".")
	m, err := strconv.Atoi(minor)
	return ok && err == nil && m >= MinMinor
}

// Badge is the picker's one-word state.
func (c Cluster) Badge() string {
	switch {
	case c.Status != string(ekstypes.ClusterStatusActive):
		return strings.ToLower(c.Status)
	case !c.Supported():
		return "needs 1." + strconv.Itoa(MinMinor) + "+"
	case c.AutoMode:
		return "ready · Auto Mode"
	}
	return "ready"
}

// EKSAPI is the slice of the EKS client the wizard uses.
type EKSAPI interface {
	ListClusters(context.Context, *eks.ListClustersInput, ...func(*eks.Options)) (*eks.ListClustersOutput, error)
	DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
}

// STSAPI is the slice of the STS client the wizard uses.
type STSAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// Client reads from one Region with one profile.
type Client struct {
	EKS EKSAPI
	STS STSAPI
}

// New loads the AWS config the way the AWS CLI does: profile, then the
// default credential chain.
func New(ctx context.Context, profile, region string) (*Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{EKS: eks.NewFromConfig(cfg), STS: sts.NewFromConfig(cfg)}, nil
}

// Identity returns the account id and caller ARN.
func (c *Client) Identity(ctx context.Context) (account, arn string, err error) {
	out, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", "", err
	}
	return aws.ToString(out.Account), aws.ToString(out.Arn), nil
}

// ListClusters lists and describes every cluster in the Region, by name.
func (c *Client) ListClusters(ctx context.Context) ([]Cluster, error) {
	var names []string
	p := eks.NewListClustersPaginator(c.EKS, &eks.ListClustersInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		names = append(names, page.Clusters...)
	}
	sort.Strings(names)
	out := make([]Cluster, 0, len(names))
	for _, n := range names {
		cl, err := c.Describe(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, nil
}

// Describe reads one cluster.
func (c *Client) Describe(ctx context.Context, name string) (Cluster, error) {
	d, err := c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(name)})
	if err != nil {
		return Cluster{}, fmt.Errorf("describe %s: %w", name, err)
	}
	k := d.Cluster
	cl := Cluster{Name: name, Version: aws.ToString(k.Version), Status: string(k.Status), Endpoint: aws.ToString(k.Endpoint)}
	if k.CertificateAuthority != nil {
		cl.CA = aws.ToString(k.CertificateAuthority.Data)
	}
	cl.AutoMode = k.ComputeConfig != nil && aws.ToBool(k.ComputeConfig.Enabled)
	return cl, nil
}

// KubeconfigPath is the installer's own kubeconfig for a cluster, like
// substrate-gke's snapshot.KubeconfigPath: the user's ~/.kube/config and its
// current context are never touched.
func KubeconfigPath(cluster, region string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "substrate-eks", "kubeconfig", cluster+"."+region+".yaml")
}

// WriteKubeconfig writes a mode-600 kubeconfig for cl. kubectl gets a token
// from `aws eks get-token`, the same exec plugin `aws eks update-kubeconfig`
// writes; nothing secret is stored in the file.
func WriteKubeconfig(path string, cl Cluster, region, profile string) error {
	if cl.Endpoint == "" || cl.CA == "" {
		return fmt.Errorf("cluster %s has no endpoint or CA yet", cl.Name)
	}
	if _, err := base64.StdEncoding.DecodeString(cl.CA); err != nil {
		return fmt.Errorf("cluster %s CA is not base64: %w", cl.Name, err)
	}
	args := []string{"eks", "get-token", "--output", "json", "--cluster-name", cl.Name, "--region", region}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	var b strings.Builder
	ctx := cl.Name
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Config\ncurrent-context: %s\n", ctx)
	fmt.Fprintf(&b, "clusters:\n- name: %s\n  cluster:\n    server: %s\n    certificate-authority-data: %s\n", ctx, cl.Endpoint, cl.CA)
	fmt.Fprintf(&b, "contexts:\n- name: %s\n  context:\n    cluster: %s\n    user: %s\n", ctx, ctx, ctx)
	fmt.Fprintf(&b, "users:\n- name: %s\n  user:\n    exec:\n      apiVersion: client.authentication.k8s.io/v1beta1\n      command: aws\n      interactiveMode: Never\n      args:\n", ctx)
	for _, a := range args {
		fmt.Fprintf(&b, "      - %q\n", a)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
