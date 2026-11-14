package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

type policyStatement struct {
	Effect    string
	Action    any
	Resource  any
	Condition map[string]map[string]string
}

func statements(t *testing.T, doc string) []policyStatement {
	t.Helper()
	var p struct{ Statement []policyStatement }
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("policy %s: %v", doc, err)
	}
	return p.Statement
}

func strs(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, s := range x {
			out = append(out, s.(string))
		}
		return out
	}
	return nil
}

// Review F3: on Auto Mode, the cluster role gets the one call the
// StorageClass tags need, and nothing wider: ec2:CreateVolume on this
// account's volumes in this Region, for this cluster, with the owner tag.
func TestAutoModeClusterRoleMayCreateTaggedVolumes(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc := f.roles["demo-us-west-2-eks-cluster"].inline[volumeTagPolicyName]
	if doc == "" {
		t.Fatalf("the Auto Mode cluster role has no %s policy", volumeTagPolicyName)
	}
	st := statements(t, doc)
	if len(st) != 1 {
		t.Fatalf("%d statements, want 1: %s", len(st), doc)
	}
	s := st[0]
	if a := strs(s.Action); s.Effect != "Allow" || len(a) != 1 || a[0] != "ec2:CreateVolume" {
		t.Errorf("effect %s, actions %v", s.Effect, a)
	}
	if r := strs(s.Resource); len(r) != 1 || r[0] != "arn:aws:ec2:us-west-2:111122223333:volume/*" {
		t.Errorf("resources %v", r)
	}
	eq := s.Condition["StringEquals"]
	if len(s.Condition) != 1 || len(eq) != 2 || eq["aws:RequestTag/eks:eks-cluster-name"] != "${aws:PrincipalTag/eks:eks-cluster-name}" ||
		eq["aws:RequestTag/"+OwnerTagKey] != "demo.us-west-2" {
		t.Errorf("conditions %v", s.Condition)
	}
}

// No role create cluster makes has a wildcard action, or may tag any
// resource (Resource "*" with ec2:CreateTags).
func TestClusterRolesInlinePoliciesHaveNoWildcards(t *testing.T) {
	for _, auto := range []string{"on", ""} {
		f := newClusterFake()
		if err := clusterTestRun(t, f, false, func(c *clusterConfig) { c.AutoMode = auto }).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		n := 0
		for role, r := range f.roles {
			for name, doc := range r.inline {
				n++
				for _, s := range statements(t, doc) {
					for _, a := range strs(s.Action) {
						if strings.Contains(a, "*") || (a == "ec2:CreateTags" && strings.Contains(strings.Join(strs(s.Resource), " "), "*")) {
							t.Errorf("auto=%q: %s policy %s allows %s on %v", auto, role, name, a, s.Resource)
						}
					}
					for _, res := range strs(s.Resource) {
						if res == "*" {
							t.Errorf("auto=%q: %s policy %s has Resource *", auto, role, name)
						}
					}
				}
			}
		}
		// Only the Auto Mode cluster role has inline policies: volume and
		// node tags.
		if want := map[string]int{"on": 2, "": 0}[auto]; n != want {
			t.Errorf("auto=%q: %d inline policies, want %d", auto, n, want)
		}
	}
}

// A run stopped right after it created the cluster role is completed: the
// rerun finds the role without the policy and puts it, once; a role whose
// policy drifted is put back.
func TestClusterRoleVolumePolicyIsReconciled(t *testing.T) {
	f := newClusterFake()
	f.failAfter = "CreateRole demo-us-west-2-eks-cluster"
	if err := clusterTestRun(t, f, false).run(context.Background()); err == nil {
		t.Fatal("the interrupted run reported success")
	}
	if f.roles["demo-us-west-2-eks-cluster"].inline[volumeTagPolicyName] != "" {
		t.Fatal("the policy was put although the run stopped after CreateRole")
	}
	r := clusterTestRun(t, f, false)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	put := "PutRolePolicy demo-us-west-2-eks-cluster " + volumeTagPolicyName
	if countCalls(f.writes, put) != 1 || f.roles["demo-us-west-2-eks-cluster"].inline[volumeTagPolicyName] != r.volumeTagPolicy() {
		t.Errorf("writes %v", f.writes)
	}

	// Drift, on a role whose cluster is not created yet.
	f = newClusterFake()
	f.failBefore = "CreateCluster"
	if err := clusterTestRun(t, f, false).run(context.Background()); err == nil {
		t.Fatal("the stopped run reported success")
	}
	f.roles["demo-us-west-2-eks-cluster"].inline[volumeTagPolicyName] = `{"Version":"2012-10-17","Statement":[]}`
	f.writes = nil
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countCalls(f.writes, put) != 1 {
		t.Errorf("a drifted policy was not put back: %v", f.writes)
	}
}

// The node tag policy: the documented compute and networking creates, on
// this account and Region's resources of the kinds they make, for this
// cluster, with the owner tag on the request.
func TestAutoModeClusterRoleMayLaunchTaggedNodes(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc := f.roles["demo-us-west-2-eks-cluster"].inline[nodeTagPolicyName]
	st := statements(t, doc)
	if len(st) != 2 {
		t.Fatalf("%d statements, want 2: %s", len(st), doc)
	}
	want := []struct{ actions, resources, like string }{
		{"ec2:CreateFleet ec2:RunInstances ec2:CreateLaunchTemplate",
			"arn:aws:ec2:us-west-2:111122223333:fleet/* arn:aws:ec2:us-west-2:111122223333:instance/* arn:aws:ec2:us-west-2:111122223333:launch-template/* " +
				"arn:aws:ec2:us-west-2:111122223333:network-interface/* arn:aws:ec2:us-west-2:111122223333:volume/*",
			"aws:RequestTag/eks:kubernetes-node-class-name aws:RequestTag/eks:kubernetes-node-pool-name"},
		{"ec2:CreateNetworkInterface", "arn:aws:ec2:us-west-2:111122223333:network-interface/*", "aws:RequestTag/eks:kubernetes-cni-node-name"},
	}
	for i, s := range st {
		var like []string
		for _, k := range sortedKeys(s.Condition["StringLike"]) {
			like = append(like, k)
		}
		eq := s.Condition["StringEquals"]
		if s.Effect != "Allow" || strings.Join(strs(s.Action), " ") != want[i].actions || strings.Join(strs(s.Resource), " ") != want[i].resources ||
			strings.Join(like, " ") != want[i].like || len(eq) != 2 ||
			eq["aws:RequestTag/eks:eks-cluster-name"] != "${aws:PrincipalTag/eks:eks-cluster-name}" || eq["aws:RequestTag/"+OwnerTagKey] != "demo.us-west-2" {
			t.Errorf("statement %d = %+v", i, s)
		}
	}
}

// Both directions for a rerun on an existing Auto Mode cluster: a missing
// node tag policy is put, once, on the cluster's owned role; a matching one
// is left alone (no write); a role another deployment owns is left as it is
// with a warning, nothing written, and the run goes on.
func TestExistingClusterRoleInlinePoliciesAreReconciled(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	role := f.roles["demo-us-west-2-eks-cluster"]
	// EKS answers DescribeCluster with the role the cluster was created with.
	f.cluster.RoleArn = aws.String(role.arn)
	// A cluster made before the node tag policy existed.
	delete(role.inline, nodeTagPolicyName)
	f.writes = nil
	r := clusterTestRun(t, f, false)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	put := "PutRolePolicy demo-us-west-2-eks-cluster " + nodeTagPolicyName
	if countCalls(f.writes, put) != 1 || len(f.writes) != 1 || role.inline[nodeTagPolicyName] != r.nodeTagPolicy() || len(r.changes) != 1 {
		t.Errorf("missing policy: writes %v, changes %v", f.writes, r.changes)
	}

	// Matching: nothing is written.
	f.writes = nil
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil || len(f.writes) != 0 {
		t.Errorf("matching policy: err %v, writes %v", err, f.writes)
	}

	// Another deployment's role: no error, nothing written.
	delete(role.inline, nodeTagPolicyName)
	role.tags[OwnerTagKey] = "other.us-west-2"
	f.writes = nil
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || role.inline[nodeTagPolicyName] != "" {
		t.Errorf("foreign role: writes %v", f.writes)
	}
}
