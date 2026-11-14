package cmd

import (
	"strings"
	"testing"
)

// The longest cluster name clusterNameRE accepts keeps every derived name
// valid in every Region (review R8): IAM role names fit 64 characters and
// bucket names 63. The longest Region codes the SDK's partitions list are
// us-isof-south-1 (15) and, in the aws partition, the 14-character
// ap-southeast-N and ap-northeast-N.
func TestDerivedNamesFitForTheLongestClusterName(t *testing.T) {
	longest := ""
	for n := 2; n <= 64; n++ {
		if name := "a" + strings.Repeat("b", n-1); clusterNameRE.MatchString(name) {
			longest = name
		}
	}
	if len(longest) < 2 || clusterNameRE.MatchString(longest+"b") {
		t.Fatalf("clusterNameRE has no maximum below 64: longest accepted %q", longest)
	}
	for _, region := range []string{"us-isof-south-1", "ap-southeast-4", "us-east-1"} {
		n := deriveNames("aws", "111122223333", region, longest)
		r := &clusterRun{e: &env{names: n}}
		// Every IAM role setup-aws makes: create cluster's, bootstrap's and
		// the plugins'.
		roles := []string{r.clusterRoleName(), r.autoNodeRoleName(), r.nodeRoleName(), r.ebsRoleName(),
			n.EFSControllerRole(), n.EFSNodeRole(), n.S3FilesSyncRole(), esoRoleName(n),
			n.IAMPrefix + "-" + llmServiceAccount, n.IAMPrefix + "-" + cpServiceAccount}
		for _, p := range r.e.podIdentities() {
			roles = append(roles, n.IAMPrefix+"-"+p.ServiceAccount)
		}
		for _, role := range roles {
			if len(role) > 64 {
				t.Errorf("%d-character cluster name in %s: IAM role %s is %d characters, over 64", len(longest), region, role, len(role))
			}
		}
		for _, b := range []string{n.SnapBucket, n.AssetBucket, n.S3FilesBucket()} {
			if len(b) > 63 {
				t.Errorf("%d-character cluster name in %s: bucket %s is %d characters, over 63", len(longest), region, b, len(b))
			}
		}
	}
}
