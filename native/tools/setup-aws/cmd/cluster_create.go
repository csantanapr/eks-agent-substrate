package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"

	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/nodegroups"
	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/phases"
)

// CreatedByTagKey marks what setup-aws itself created, next to the owner tag,
// so teardown can tell these resources from the ones eksctl made for the
// same deployment.
const (
	CreatedByTagKey   = "agent-substrate-on-eks/created-by"
	CreatedByTagValue = "setup-aws"
)

// clusterConfig holds the `create cluster` settings. Each flag's default
// comes from the config.env variable named in its help.
type clusterConfig struct {
	KubernetesVersion string
	// AutoMode is "on", or "" / "off" for a standard cluster (config.env:
	// AUTO_MODE empty means no Auto Mode).
	AutoMode string
	// PublicAccessCIDRs is EKS_PUBLIC_ACCESS_CIDRS, space separated, or
	// publicAccessAuto (the default): this machine's address as a /32.
	PublicAccessCIDRs                                           string
	SystemInstanceType, GvisorInstanceType, MicrovmInstanceType string
	SystemNodes, GvisorNodes, MicrovmNodes                      int
	// NodeGroups is NODE_GROUPS: the node groups to make, comma-separated,
	// or "none" (nodegroups.Parse); nodegroups.Check is the rule.
	NodeGroups string
	// NodeAMIID is NODE_AMI_ID: the AMI of the node groups to make, or ""
	// for the Amazon EKS optimized Amazon Linux 2023 AMI EKS picks.
	NodeAMIID string
	// NodeGroupType is NODE_GROUP_TYPE (nodegroups.Types): EKS managed node
	// groups, or self-managed ones (selfmanaged.go). Empty is managed.
	NodeGroupType string
	// The nitro node group's (NODE_GROUPS names it for the AWS Nitro sandbox
	// class with NITRO_COMPUTE=node-group; nitro.go): its AMI, NITRO_AMI_ID,
	// a Nitro Sandbox AMI and never NODE_AMI_ID; its instance type, which
	// must support Nitro Enclaves; and its nodes.
	NitroAMIID, NitroInstanceType string
	NitroNodes                    int
	// Karpenter is KARPENTER: "on" makes the gvisor group NODE_GROUPS
	// names a Karpenter NodePool (karpenter.go), on a new cluster without
	// Auto Mode; "off" or empty makes it a node group.
	Karpenter string
	// KarpenterNodeOverlay is KARPENTER_NODE_OVERLAY: "on", with
	// KARPENTER=on, turns on Karpenter's alpha NodeOverlay and makes the
	// microvm and nitro groups NODE_GROUPS names NodePools too, each with a
	// NodeOverlay; "off" or empty leaves them node groups.
	KarpenterNodeOverlay string
}

var ccfg clusterConfig

var createClusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Create the VPC, IAM roles, EKS cluster, add-ons and node groups",
	Long: `Creates, through the AWS SDK: a VPC with
public and private subnets in 2-3 Availability Zones and one NAT gateway; the
cluster and node IAM roles; the EKS cluster (Auto Mode on by default) with its
API endpoint limited to EKS_PUBLIC_ACCESS_CIDRS (by default "auto": this
machine's public address, a /32, looked up at https://checkip.amazonaws.com
for a new cluster; an explicit value is applied to an existing one), and a
check that this machine reaches it; a cluster-admin access entry
for the caller; the add-ons; and the node groups NODE_GROUPS names (by
default system, gvisor and microvm), each with its launch template (IMDSv2
only, hop limit 1, encrypted gp3, nested virtualization on microvm, Nitro
Enclaves on nitro): EKS managed node groups, or with
NODE_GROUP_TYPE=self-managed an Auto Scaling group each, with an instance
profile and an access entry for the node role. The nitro group, for the AWS
Nitro sandbox on node groups, runs the Nitro Sandbox AMI NITRO_AMI_ID, and
the run waits until its nodes advertise Nitro sandbox capacity. Without Auto
Mode the system group and a gvisor, microvm or nitro group are required.
With it any of them, or none, will do: Auto Mode's nodes run what no group
is made for, and NODE_GROUPS=none makes no node group (and no node IAM
role). With KARPENTER=on (a new cluster without Auto Mode), the gvisor group
is a Karpenter NodePool instead: the run makes Karpenter's IAM role, its
interruption queue and EventBridge rules, then installs Karpenter on the
system group and the pool's EC2NodeClass and NodePool, through the
Kubernetes API. The microvm and nitro groups stay node groups, unless
KARPENTER_NODE_OVERLAY=on: then they are NodePools too, each with a
NodeOverlay (an alpha Karpenter API) that tells Karpenter the extended
resource their worker Pods request. Idempotent: a rerun creates only what
is missing, and --plan changes nothing.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		e, err := newEnv(cmd.Context())
		if err != nil {
			return err
		}
		c, err := newClusterClients(cmd.Context(), e.cfg)
		if err != nil {
			return err
		}
		return newClusterRun(e, c, ccfg).run(cmd.Context())
	},
}

func init() {
	f := createClusterCmd.Flags()
	f.StringVar(&ccfg.KubernetesVersion, "kubernetes-version", getEnv("KUBERNETES_VERSION", "1.37"), "EKS version of a new cluster (KUBERNETES_VERSION)")
	f.StringVar(&ccfg.AutoMode, "auto-mode", autoModeDefault(), `"on": EKS Auto Mode with the general-purpose and system NodePools next to the node groups; "off" or empty: a standard cluster (AUTO_MODE)`)
	f.StringVar(&ccfg.PublicAccessCIDRs, "public-access-cidrs", getEnv("EKS_PUBLIC_ACCESS_CIDRS", publicAccessAuto), `space-separated address ranges allowed to reach the public API endpoint, or "auto": this machine's public address as a /32 (EKS_PUBLIC_ACCESS_CIDRS)`)
	f.StringVar(&ccfg.SystemInstanceType, "system-instance-type", getEnv("SYSTEM_INSTANCE_TYPE", "m8i.xlarge"), "instance type of the system node group (SYSTEM_INSTANCE_TYPE)")
	f.StringVar(&ccfg.GvisorInstanceType, "gvisor-instance-type", getEnv("GVISOR_INSTANCE_TYPE", "m8i.2xlarge"), "instance type of the gvisor node group (GVISOR_INSTANCE_TYPE)")
	f.StringVar(&ccfg.MicrovmInstanceType, "microvm-instance-type", getEnv("MICROVM_INSTANCE_TYPE", "m8i.2xlarge"), "instance type of the microvm node group; needs nested virtualization (MICROVM_INSTANCE_TYPE)")
	f.IntVar(&ccfg.SystemNodes, "system-nodes", getEnv("SYSTEM_NODES", 2), "nodes in the system node group (SYSTEM_NODES)")
	f.IntVar(&ccfg.GvisorNodes, "gvisor-nodes", getEnv("GVISOR_NODES", 2), "nodes in the gvisor node group (GVISOR_NODES)")
	f.IntVar(&ccfg.MicrovmNodes, "microvm-nodes", getEnv("MICROVM_NODES", 2), "nodes in the microvm node group (MICROVM_NODES)")
	f.StringVar(&ccfg.NodeGroups, "node-groups", getEnv("NODE_GROUPS", nodegroups.Default),
		`node groups to make, comma-separated (system, gvisor, microvm, nitro), or "none" on an Auto Mode cluster (NODE_GROUPS)`)
	f.StringVar(&ccfg.NodeAMIID, "node-ami-id", getEnv("NODE_AMI_ID", ""),
		`AMI of the node groups to make, one that joins with nodeadm (Amazon Linux 2023); empty: the Amazon EKS optimized AL2023 AMI (NODE_AMI_ID)`)
	f.StringVar(&ccfg.NodeGroupType, "node-group-type", getEnv("NODE_GROUP_TYPE", nodegroups.Managed),
		`"managed": EKS managed node groups; "self-managed": an Auto Scaling group per node group, whose nodes join with nodeadm (NODE_GROUP_TYPE)`)
	f.StringVar(&ccfg.NitroAMIID, "nitro-ami-id", getEnv("NITRO_AMI_ID", ""),
		"AMI of the nitro node group: a Nitro Sandbox AMI built with awslabs/amazon-eks-ami (enable_pod_isolation=true); required when NODE_GROUPS names nitro (NITRO_AMI_ID)")
	f.StringVar(&ccfg.NitroInstanceType, "nitro-instance-type", getEnv("NITRO_INSTANCE_TYPE", "m8i.xlarge"), "instance type of the nitro node group; needs Nitro Enclaves (NITRO_INSTANCE_TYPE)")
	f.IntVar(&ccfg.NitroNodes, "nitro-nodes", getEnv("NITRO_NODES", 1), "nodes in the nitro node group (NITRO_NODES)")
	f.StringVar(&ccfg.Karpenter, "karpenter", getEnv("KARPENTER", ""),
		`"on": Karpenter `+nodegroups.KarpenterVersion+` makes the gvisor nodes in a NodePool of up to GVISOR_NODES nodes, on a new cluster without Auto Mode; the microvm and nitro groups stay node groups; "off" or empty: a gvisor node group (KARPENTER)`)
	f.StringVar(&ccfg.KarpenterNodeOverlay, "karpenter-node-overlay", getEnv("KARPENTER_NODE_OVERLAY", ""),
		`"on", with KARPENTER=on: Karpenter's alpha NodeOverlay on, and the microvm and nitro nodes in NodePools too, of up to MICROVM_NODES and NITRO_NODES nodes; "off" or empty: microvm and nitro node groups (KARPENTER_NODE_OVERLAY)`)
	createCmd.AddCommand(createClusterCmd)
}

// autoModeDefault reads AUTO_MODE itself: getEnv treats an empty value as
// unset, but config.env's AUTO_MODE= (empty) means Auto Mode off.
func autoModeDefault() string {
	if v, ok := os.LookupEnv("AUTO_MODE"); ok {
		return v
	}
	return "on"
}

// clusterRun is one `create cluster` run: the settings, the clients, and
// what the phases have found or created so far.
type clusterRun struct {
	e    *env
	c    *ClusterClients
	cfg  clusterConfig
	auto bool
	// karpenter is KARPENTER=on: the gvisor group is a Karpenter NodePool
	// (pools), not a node group (groups).
	karpenter bool
	// nodeOverlay is KARPENTER_NODE_OVERLAY=on: the microvm and nitro
	// groups are Karpenter NodePools too (nodegroups.KarpenterPools), each
	// with a NodeOverlay, since their worker Pods request ate.dev/kvm and
	// eks.amazonaws.com/nitro-sandbox, which only a NodeOverlay tells
	// Karpenter about (karpenter_overlay.go).
	nodeOverlay bool
	// autoCIDRs is EKS_PUBLIC_ACCESS_CIDRS=auto: a new cluster gets this
	// machine's /32 (resolvePublicAccess), an existing one keeps its ranges.
	autoCIDRs bool
	// tags are the owner tag, TAGS and the created-by tag.
	tags map[string]string

	creates, changes []string

	// wanted are the node groups NODE_GROUPS names; wantedErr is why it
	// cannot be read, which validate reports.
	wanted    []string
	wantedErr error

	cluster    *ekstypes.Cluster // nil while it does not exist
	nodegroups map[string]*ekstypes.Nodegroup
	addons     map[string]*ekstypes.Addon
	azs        []string
	amiTypes   map[string]ekstypes.AMITypes // by instance type
	// enaRequired are the instance types that run only AMIs with ENA.
	enaRequired map[string]bool
	image       *nodeImage // NODE_AMI_ID, when set
	// nitroImage is NITRO_AMI_ID, read when the nitro group is wanted.
	nitroImage *nodeImage
	// archImages are the AMIs of self-managed groups without NODE_AMI_ID,
	// by architecture: the recommended EKS optimized AL2023 AMI.
	archImages map[ec2types.ArchitectureValues]*nodeImage
	// asgs are the Auto Scaling groups of this deployment's self-managed
	// node groups that exist, by node group.
	asgs      map[string]*asgtypes.AutoScalingGroup
	vpcID     string
	eipID     string
	public    []string // subnet IDs
	private   []string
	templates map[string]ltRef // by node group

	// vcpus are the vCPUs of each instance type, by type, for the
	// NodePools' CPU limit.
	vcpus map[string]int32
	// poolImages are the AMIs of the Karpenter NodePools, by pool: the AMI
	// an existing EC2NodeClass names (seeded), else NODE_AMI_ID or the
	// recommended EKS optimized AL2023 AMI. seeded are the pools whose
	// EC2NodeClass exists.
	poolImages map[string]*nodeImage
	seeded     map[string]bool
}

func newClusterRun(e *env, c *ClusterClients, cfg clusterConfig) *clusterRun {
	tags := map[string]string{CreatedByTagKey: CreatedByTagValue}
	for k, v := range e.tags {
		tags[k] = v
	}
	if strings.TrimSpace(cfg.NodeGroupType) == "" {
		cfg.NodeGroupType = nodegroups.Managed
	}
	r := &clusterRun{e: e, c: c, cfg: cfg, auto: cfg.AutoMode == "on", karpenter: cfg.Karpenter == nodegroups.KarpenterOn,
		nodeOverlay: cfg.KarpenterNodeOverlay == nodegroups.KarpenterOn, tags: tags,
		autoCIDRs:  strings.TrimSpace(cfg.PublicAccessCIDRs) == publicAccessAuto,
		nodegroups: map[string]*ekstypes.Nodegroup{}, addons: map[string]*ekstypes.Addon{},
		amiTypes: map[string]ekstypes.AMITypes{}, enaRequired: map[string]bool{}, templates: map[string]ltRef{},
		archImages: map[ec2types.ArchitectureValues]*nodeImage{}, asgs: map[string]*asgtypes.AutoScalingGroup{},
		vcpus: map[string]int32{}, poolImages: map[string]*nodeImage{}, seeded: map[string]bool{}}
	// Empty is unset, as getEnv reads NODE_GROUPS: the default three.
	ng := cfg.NodeGroups
	if strings.TrimSpace(ng) == "" {
		ng = nodegroups.Default
	}
	r.wanted, r.wantedErr = nodegroups.Parse(ng)
	return r
}

func (r *clusterRun) run(ctx context.Context) error {
	if r.e.cfg.Plan {
		slog.Info("plan mode: reading only; nothing will be created or changed")
	}
	slog.Info("Creating the cluster...", "cluster", r.e.names.Cluster, "region", r.e.names.Region, "autoMode", r.auto)
	steps := []func(context.Context) error{
		r.preflight,
		r.ensureNetwork,
		r.ensureRoles,
		r.ensureCluster,
		r.ensureAccess,
		r.ensureAddons,
		r.ensureLaunchTemplates,
		r.ensureNodegroups,
		r.waitReady,
	}
	if r.selfManaged() {
		steps[7], steps[8] = r.ensureAutoScalingGroups, r.waitSelfManaged
	}
	if r.karpenter {
		steps = append(steps, r.ensureKarpenterAWS, r.ensureKarpenterKube)
	}
	list := phases.ClusterFor(r.cfg.NodeGroupType, r.karpenter)
	if len(steps) != len(list) {
		return fmt.Errorf("internal: %d cluster steps for %d phases", len(steps), len(list))
	}
	start := time.Now()
	for i, step := range steps {
		slog.Info(phases.Line(list, i))
		t := time.Now()
		if err := step(ctx); err != nil {
			return err
		}
		slog.Info("phase done", "step", i+1, "took", time.Since(t).Round(time.Second).String())
	}
	if r.e.cfg.Plan {
		slog.Info("plan: done", "wouldCreate", len(r.creates), "wouldChange", len(r.changes))
		return nil
	}
	slog.Info("Cluster ready.", "cluster", r.e.names.Cluster, "created", len(r.creates), "changed", len(r.changes),
		"took", time.Since(start).Round(time.Second).String())
	return nil
}

// create runs a create through env.act (so --plan skips it) and counts it.
func (r *clusterRun) create(what string, do func() error) error {
	r.creates = append(r.creates, what)
	return r.e.act("create "+what, do)
}

// change runs any other write through env.act and counts it.
func (r *clusterRun) change(what string, do func() error) error {
	r.changes = append(r.changes, what)
	return r.e.act(what, do)
}

func (r *clusterRun) plan() bool { return r.e.cfg.Plan }

// planned marks the ID of a resource that --plan would create.
const plannedPrefix = "planned-"

func planned(id string) bool { return strings.HasPrefix(id, plannedPrefix) }

// --- phase 1: preflight -------------------------------------------------------

// clusterNameRE is the CLUSTER_NAME rule, 2-21 characters. IAM role names
// are the binding limit: <cluster>-<region>-<suffix> must fit IAM's 64
// characters, and 21 + 1 + 15 + 1 + 26 = 64 for the longest suffix
// (aws-sm-credential-provider) in the longest Region code the SDK's
// partitions list (us-isof-south-1). Buckets (63, with the 12-digit account
// and the Region) and the other derived names then fit too;
// TestDerivedNamesFitForTheLongestClusterName checks every one.
var clusterNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}[a-z0-9]$`)

// eksUnsupportedAZIDs are the zones EKS refuses cluster subnets in.
// https://docs.aws.amazon.com/eks/latest/userguide/network-reqs.html
var eksUnsupportedAZIDs = []string{"use1-az3", "usw1-az2", "cac1-az3"}

// maxAZs is how many zones the new VPC spans (eksctl's default too).
const maxAZs = 3

func (r *clusterRun) preflight(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if err := r.readCluster(ctx); err != nil {
		return err
	}
	if err := r.requireNitroAMI(); err != nil {
		return err
	}
	if err := r.checkGroupType(ctx); err != nil {
		return err
	}
	if err := r.checkInstanceTypes(ctx); err != nil {
		return err
	}
	if err := r.checkImage(ctx); err != nil {
		return err
	}
	if err := r.checkPoolImages(ctx); err != nil {
		return err
	}
	if r.cluster == nil {
		// auto picks a new cluster's ranges only; an existing cluster's
		// stay as they are (ensurePublicAccess).
		if err := r.resolvePublicAccess(ctx); err != nil {
			return err
		}
		if err := r.pickAZs(ctx); err != nil {
			return err
		}
		if err := r.checkQuotas(ctx); err != nil {
			return err
		}
	}
	if strings.TrimSpace(r.cfg.PublicAccessCIDRs) == "0.0.0.0/0" {
		slog.Warn("the Kubernetes API accepts connections from any address (authenticated by IAM); set EKS_PUBLIC_ACCESS_CIDRS to narrow it")
	}
	return nil
}

func (r *clusterRun) validate() error {
	if !clusterNameRE.MatchString(r.e.names.Cluster) {
		return fmt.Errorf("cluster name %q must be 2-21 characters of a-z, 0-9 and '-', starting with a letter", r.e.names.Cluster)
	}
	switch r.cfg.AutoMode {
	case "on", "off", "":
	default:
		return fmt.Errorf(`AUTO_MODE must be "on", "off" or empty, not %q`, r.cfg.AutoMode)
	}
	if err := nodegroups.CheckType(r.cfg.NodeGroupType); err != nil {
		return err
	}
	if err := versionAtLeast(r.cfg.KubernetesVersion, r.e.cfg.MinKubernetesMinor); err != nil {
		return fmt.Errorf("KUBERNETES_VERSION: %w", err)
	}
	cidrs := strings.Fields(r.cfg.PublicAccessCIDRs)
	if len(cidrs) == 0 {
		return errors.New("EKS_PUBLIC_ACCESS_CIDRS is empty; give at least one address range")
	}
	for _, c := range cidrs {
		if _, _, err := net.ParseCIDR(c); err != nil && !r.autoCIDRs {
			return fmt.Errorf("EKS_PUBLIC_ACCESS_CIDRS: %q is not an address range", c)
		}
	}
	if r.wantedErr != nil {
		return r.wantedErr
	}
	if err := nodegroups.Check(r.auto, r.wanted); err != nil {
		return fmt.Errorf("NODE_GROUPS=%s: %w", nodegroups.Format(r.wanted), err)
	}
	for _, g := range r.groups() {
		if g.Desired < 1 {
			return fmt.Errorf("the %s node group needs at least one node", g.Name)
		}
	}
	return r.validateKarpenter()
}

// publicAccessAuto is EKS_PUBLIC_ACCESS_CIDRS's default: only this machine
// may reach the Kubernetes API, from the address it has when this runs.
const publicAccessAuto = "auto"

// cidrsFix is how to fix a refused auto or an unreachable API.
const cidrsFix = "set EKS_PUBLIC_ACCESS_CIDRS to your network's egress ranges (for example 203.0.113.0/24), or to 0.0.0.0/0 (IAM still authenticates every call)"

// Replaced in tests.
var (
	checkIPURL     = "https://checkip.amazonaws.com"
	checkIPTimeout = 5 * time.Second
	// checkIPSamples lookups, each on a new connection, must agree.
	checkIPSamples = 5
)

// resolvePublicAccess turns EKS_PUBLIC_ACCESS_CIDRS=auto into this
// machine's public IPv4 address as a /32, for a cluster this run creates. A
// failed lookup stops the run: it never falls back to 0.0.0.0/0.
//
// A NAT with several public addresses picks one per connection, so the API
// would see this machine from any of them: the lookup is made
// checkIPSamples times on new connections, and answers that differ refuse
// auto. A network may still reach AWS from yet another address (a separate
// route to AWS); probeAPI catches that once the cluster is up.
//
// When this machine's address changes later (another network, a new DHCP
// lease), kubectl and the installer time out reaching the API, while the EKS
// API itself, which these ranges do not limit, still works. auto never
// changes an existing cluster's ranges: set EKS_PUBLIC_ACCESS_CIDRS and
// rerun `setup-aws create cluster`, and ensurePublicAccess applies it.
func (r *clusterRun) resolvePublicAccess(ctx context.Context) error {
	if !r.autoCIDRs {
		return nil
	}
	// Go's client, like kubectl and the AWS SDK, honours HTTPS_PROXY.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	client := &http.Client{Transport: t}
	seen := map[string]bool{}
	var addrs []string
	for range checkIPSamples {
		a, err := callerAddress(ctx, client)
		if err != nil {
			return fmt.Errorf("EKS_PUBLIC_ACCESS_CIDRS=auto: %w; %s", err, cidrsFix)
		}
		if !seen[a] {
			seen[a] = true
			addrs = append(addrs, a)
		}
	}
	if len(addrs) > 1 {
		return fmt.Errorf("EKS_PUBLIC_ACCESS_CIDRS=auto: this network reaches the internet from %d addresses (%s in %d lookups at %s), so no single /32 lets this machine reach the Kubernetes API; %s",
			len(addrs), strings.Join(addrs, ", "), checkIPSamples, checkIPURL, cidrsFix)
	}
	r.cfg.PublicAccessCIDRs = addrs[0]
	slog.Info("the Kubernetes API will accept connections from this machine's address only; set EKS_PUBLIC_ACCESS_CIDRS to allow more", "address", addrs[0])
	return nil
}

// callerAddress asks checkIPURL for this machine's public IPv4 address and
// returns it as a /32.
func callerAddress(ctx context.Context, client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkIPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkIPURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("look up this machine's public address: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("look up this machine's public address at %s: %s", checkIPURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", fmt.Errorf("look up this machine's public address: %w", err)
	}
	// An IPv4 cluster's endpoint takes IPv4 ranges only.
	ip, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return "", fmt.Errorf("%s did not answer with a public IPv4 address", checkIPURL)
	}
	return netip.PrefixFrom(ip, 32).String(), nil
}

// readCluster reads an existing cluster, its node groups and add-ons. The
// cluster must carry this deployment's owner tag and match AUTO_MODE.
func (r *clusterRun) readCluster(ctx context.Context) error {
	name := r.e.names.Cluster
	out, err := r.c.EKS.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(name)})
	if err != nil {
		if isNotFound(err) {
			slog.Info("no cluster yet", "cluster", name)
			return nil
		}
		return fmt.Errorf("describe cluster %s: %w", name, err)
	}
	c := out.Cluster
	if got := c.Tags[OwnerTagKey]; got != r.e.names.OwnerValue {
		return fmt.Errorf("EKS cluster %s exists but is not owned by %s (%s=%q); pick another CLUSTER_NAME", name, r.e.names.OwnerValue, OwnerTagKey, got)
	}
	switch c.Status {
	case ekstypes.ClusterStatusFailed, ekstypes.ClusterStatusDeleting:
		return fmt.Errorf("cluster %s is %s; delete it and run again", name, c.Status)
	}
	auto := c.ComputeConfig != nil && aws.ToBool(c.ComputeConfig.Enabled)
	if auto != r.auto {
		return fmt.Errorf("cluster %s has Auto Mode %s, but AUTO_MODE=%q; set AUTO_MODE to match, or delete the cluster and start again", name, onOff(auto), r.cfg.AutoMode)
	}
	if err := r.checkKarpenterMarker(c); err != nil {
		return err
	}
	if v := aws.ToString(c.Version); v != r.cfg.KubernetesVersion {
		slog.Warn("the cluster runs another Kubernetes version than KUBERNETES_VERSION; keeping it", "cluster", v, "configured", r.cfg.KubernetesVersion)
	}
	r.cluster = c
	slog.Info("cluster exists", "cluster", name, "status", c.Status, "version", aws.ToString(c.Version))
	for _, g := range r.groups() {
		ng, err := r.c.EKS.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: aws.String(name), NodegroupName: aws.String(g.Name)})
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("describe node group %s: %w", g.Name, err)
		}
		if err == nil {
			r.nodegroups[g.Name] = ng.Nodegroup
		}
	}
	for _, a := range clusterAddons {
		ad, err := r.c.EKS.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: aws.String(name), AddonName: aws.String(a.Name)})
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("describe add-on %s: %w", a.Name, err)
		}
		if err == nil {
			r.addons[a.Name] = ad.Addon
		}
	}
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// checkInstanceTypes: the microvm type must support nested virtualization
// (Kata needs /dev/kvm on a regular instance), the nitro type Nitro Enclaves
// (the Nitro Sandbox AMI needs them on), and each type's architecture picks
// its node group's AMI type. The Karpenter NodePools' types are checked the
// same way, and their vCPUs read for the pools' CPU limit.
func (r *clusterRun) checkInstanceTypes(ctx context.Context) error {
	var types []ec2types.InstanceType
	for _, g := range r.computeSpecs() {
		if t := ec2types.InstanceType(g.InstanceType); !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	if len(types) == 0 {
		// No node group: Auto Mode picks its own instance types. (With no
		// InstanceTypes, DescribeInstanceTypes would list every type.)
		slog.Info("no node group to make: Auto Mode's nodes run everything")
		return nil
	}
	out, err := r.c.EC2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: types})
	if err != nil {
		return fmt.Errorf("describe instance types: %w", err)
	}
	info := map[string]ec2types.InstanceTypeInfo{}
	for _, it := range out.InstanceTypes {
		info[string(it.InstanceType)] = it
	}
	for _, g := range r.computeSpecs() {
		it, ok := info[g.InstanceType]
		if !ok || it.ProcessorInfo == nil {
			return fmt.Errorf("instance type %s (%s) is not offered in %s", g.InstanceType, g.noun(), r.e.names.Region)
		}
		if g.NestedVirt && !slices.Contains(it.ProcessorInfo.SupportedFeatures, ec2types.SupportedAdditionalProcessorFeatureNestedVirtualization) {
			return fmt.Errorf("%s does not support nested virtualization, which the %s needs; list the types that do with:\n  aws ec2 describe-instance-types --filters Name=processor-info.supported-features,Values=nested-virtualization --query 'InstanceTypes[].InstanceType'", g.InstanceType, g.noun())
		}
		if g.Enclave && it.NitroEnclavesSupport != ec2types.NitroEnclavesSupportSupported {
			return fmt.Errorf("%s does not support Nitro Enclaves, which the %s needs (NITRO_INSTANCE_TYPE); list the types that do with:\n  aws ec2 describe-instance-types --filters Name=nitro-enclaves-support,Values=supported --query 'InstanceTypes[].InstanceType'", g.InstanceType, g.noun())
		}
		r.enaRequired[g.InstanceType] = it.NetworkInfo != nil && it.NetworkInfo.EnaSupport == ec2types.EnaSupportRequired
		if g.Pool {
			if it.VCpuInfo == nil || aws.ToInt32(it.VCpuInfo.DefaultVCpus) < 1 {
				return fmt.Errorf("instance type %s (%s) reports no vCPU count, which the NodePool's CPU limit needs", g.InstanceType, g.noun())
			}
			r.vcpus[g.InstanceType] = aws.ToInt32(it.VCpuInfo.DefaultVCpus)
		}
		switch {
		case slices.Contains(it.ProcessorInfo.SupportedArchitectures, ec2types.ArchitectureTypeX8664):
			r.amiTypes[g.InstanceType] = ekstypes.AMITypesAl2023X8664Standard
		case slices.Contains(it.ProcessorInfo.SupportedArchitectures, ec2types.ArchitectureTypeArm64):
			r.amiTypes[g.InstanceType] = ekstypes.AMITypesAl2023Arm64Standard
		default:
			return fmt.Errorf("instance type %s has no x86_64 or arm64 support", g.InstanceType)
		}
	}
	slog.Info("instance types ok", "types", fmt.Sprint(types))
	return nil
}

// pickAZs chooses up to maxAZs zones, in name order, that offer every node
// group's instance type and that EKS accepts cluster subnets in.
func (r *clusterRun) pickAZs(ctx context.Context) error {
	zones, err := r.c.EC2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{Filters: []ec2types.Filter{
		{Name: aws.String("zone-type"), Values: []string{"availability-zone"}},
		{Name: aws.String("state"), Values: []string{"available"}},
	}})
	if err != nil {
		return fmt.Errorf("describe Availability Zones: %w", err)
	}
	var types []string
	for _, g := range r.computeSpecs() {
		if !slices.Contains(types, g.InstanceType) {
			types = append(types, g.InstanceType)
		}
	}
	offered := map[string]map[string]bool{} // zone -> instance types
	p := ec2.NewDescribeInstanceTypeOfferingsPaginator(r.c.EC2, &ec2.DescribeInstanceTypeOfferingsInput{
		LocationType: ec2types.LocationTypeAvailabilityZone,
		Filters:      []ec2types.Filter{{Name: aws.String("instance-type"), Values: types}},
	})
	// With no node group there is no instance type to offer: every zone EKS
	// accepts is fine, and Auto Mode picks types per zone.
	for len(types) > 0 && p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("describe instance type offerings: %w", err)
		}
		for _, o := range page.InstanceTypeOfferings {
			z := aws.ToString(o.Location)
			if offered[z] == nil {
				offered[z] = map[string]bool{}
			}
			offered[z][string(o.InstanceType)] = true
		}
	}
	var names []string
	for _, z := range zones.AvailabilityZones {
		if slices.Contains(eksUnsupportedAZIDs, aws.ToString(z.ZoneId)) {
			continue
		}
		ok := true
		for _, t := range types {
			ok = ok && offered[aws.ToString(z.ZoneName)][t]
		}
		if ok {
			names = append(names, aws.ToString(z.ZoneName))
		}
	}
	sort.Strings(names)
	if len(names) < 2 {
		return fmt.Errorf("only %d Availability Zones in %s offer all of %s; EKS needs two. Pick other instance types or another Region", len(names), r.e.names.Region, strings.Join(types, ", "))
	}
	if len(names) > maxAZs {
		names = names[:maxAZs]
	}
	r.azs = names
	slog.Info("Availability Zones", "zones", strings.Join(names, ","))
	return nil
}

// Service Quotas codes of the two quotas a new VPC uses up.
const (
	quotaVPCs = "L-F678F1CE" // vpc: VPCs per Region
	quotaEIPs = "L-0263D0A3" // ec2: EC2-VPC Elastic IPs
)

// checkQuotas finds this deployment's VPC and NAT address from an earlier
// run, and checks that the account has room for the ones still to create.
func (r *clusterRun) checkQuotas(ctx context.Context) error {
	var err error
	if r.vpcID, err = r.findVPC(ctx); err != nil {
		return err
	}
	if r.eipID, err = r.findEIP(ctx); err != nil {
		return err
	}
	if r.vpcID == "" {
		used := 0
		p := ec2.NewDescribeVpcsPaginator(r.c.EC2, &ec2.DescribeVpcsInput{})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return fmt.Errorf("count VPCs: %w", err)
			}
			used += len(page.Vpcs)
		}
		if err := r.headroom(ctx, "vpc", quotaVPCs, "VPCs", used); err != nil {
			return err
		}
	}
	if r.eipID == "" {
		out, err := r.c.EC2.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{Filters: []ec2types.Filter{
			{Name: aws.String("domain"), Values: []string{"vpc"}}}})
		if err != nil {
			return fmt.Errorf("count Elastic IP addresses: %w", err)
		}
		if err := r.headroom(ctx, "ec2", quotaEIPs, "Elastic IP addresses", len(out.Addresses)); err != nil {
			return err
		}
	}
	return nil
}

// headroom fails unless one more of a quota's resource fits.
func (r *clusterRun) headroom(ctx context.Context, service, code, what string, used int) error {
	var limit float64
	out, err := r.c.Quotas.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: aws.String(service), QuotaCode: aws.String(code)})
	switch {
	case err == nil:
		limit = aws.ToFloat64(out.Quota.Value)
	case apiCode(err) == "NoSuchResourceException":
		// No applied value: the account has the AWS default.
		d, derr := r.c.Quotas.GetAWSDefaultServiceQuota(ctx, &servicequotas.GetAWSDefaultServiceQuotaInput{ServiceCode: aws.String(service), QuotaCode: aws.String(code)})
		if derr != nil {
			return fmt.Errorf("read the default %s quota: %w", what, derr)
		}
		limit = aws.ToFloat64(d.Quota.Value)
	default:
		return fmt.Errorf("read the %s quota: %w", what, err)
	}
	if float64(used+1) > limit {
		return fmt.Errorf("%d of %d %s are in use in %s and the cluster needs one more; release one, or request an increase of %s quota %s in the Service Quotas console", used, int(limit), what, r.e.names.Region, service, code)
	}
	slog.Info("quota ok", "quota", what, "used", used, "limit", int(limit))
	return nil
}

// apiCode is the AWS error code of err, or "".
func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// --- helpers ------------------------------------------------------------------

// pollEvery is how often a wait reads the resource again; tests set it to 0.
var pollEvery = 15 * time.Second

// waitFor calls check until it reports done, fails, or timeout passes.
func waitFor(ctx context.Context, what string, timeout time.Duration, check func() (bool, error)) error {
	slog.Info("waiting for "+what, "upTo", timeout.String())
	deadline := time.Now().Add(timeout)
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// retryNewRole retries do while EKS cannot use an IAM role yet: a role
// created seconds ago is not visible to EKS everywhere at once.
func retryNewRole(ctx context.Context, do func() error) error {
	for i := 0; ; i++ {
		err := do()
		var ae smithy.APIError
		if err == nil || i >= 20 || !errors.As(err, &ae) || ae.ErrorCode() != "InvalidParameterException" ||
			!strings.Contains(strings.ToLower(ae.ErrorMessage()), "role") {
			return err
		}
		slog.Info("the new IAM role is not usable by EKS yet; retrying", "error", ae.ErrorMessage())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// ec2Tags are r.tags plus extra, sorted, as EC2 tags.
func (r *clusterRun) ec2Tags(extra map[string]string) []ec2types.Tag {
	m := map[string]string{}
	for k, v := range r.tags {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	out := make([]ec2types.Tag, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

// tagSpec tags a resource at create time with r.tags and a Name.
func (r *clusterRun) tagSpec(t ec2types.ResourceType, name string) []ec2types.TagSpecification {
	return []ec2types.TagSpecification{{ResourceType: t, Tags: r.ec2Tags(map[string]string{"Name": name})}}
}

func ec2TagMap(tags []ec2types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// tagFilters select what setup-aws created for this deployment.
func (r *clusterRun) tagFilters(extra ...ec2types.Filter) []ec2types.Filter {
	return append([]ec2types.Filter{
		{Name: aws.String("tag:" + OwnerTagKey), Values: []string{r.e.names.OwnerValue}},
		{Name: aws.String("tag:" + CreatedByTagKey), Values: []string{CreatedByTagValue}},
	}, extra...)
}

func filter(name string, values ...string) ec2types.Filter {
	return ec2types.Filter{Name: aws.String(name), Values: values}
}

// addMissingTags tags an EC2 resource AWS made for us (the VPC's main route
// table, default security group and network ACL; the cluster security
// group), which cannot be tagged at create time. It writes only when a tag
// is missing or different.
func (r *clusterRun) addMissingTags(ctx context.Context, what, id string, have []ec2types.Tag) error {
	cur := ec2TagMap(have)
	var add []ec2types.Tag
	for _, k := range sortedKeys(r.tags) {
		if cur[k] != r.tags[k] {
			add = append(add, ec2types.Tag{Key: aws.String(k), Value: aws.String(r.tags[k])})
		}
	}
	if len(add) == 0 {
		return nil
	}
	return r.change("tag "+what+" "+id, func() error {
		_, err := r.c.EC2.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: add})
		return err
	})
}
