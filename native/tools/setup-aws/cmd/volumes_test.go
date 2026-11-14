package cmd

import (
	"context"
	"testing"
)

func pvcTags(cluster, ns string, extra ...string) map[string]string {
	m := map[string]string{eksClusterNameTag: cluster, pvcNamespaceTag: ns, "CSIVolumeName": "pvc-1", "ebs.csi.eks.amazonaws.com/cluster": "true"}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

// Review F3: inventory and teardown see this deployment's PVC volumes, the
// owner-tagged one and the ate-system one Auto Mode made untagged on a
// cluster whose role cannot add tags, and leave every other detached volume:
// a user's Retain volume in another namespace of the same cluster, another
// cluster's, and one with another owner.
func TestTeardownFindsOnlyThisDeploymentsVolumes(t *testing.T) {
	f := fullDeployment()
	f.put(&tdObj{kind: kVolume, id: "vol-auto-pg", state: "available", tags: pvcTags("demo", "ate-system")})
	f.put(&tdObj{kind: kVolume, id: "vol-user-retain", state: "available", tags: pvcTags("demo", "myapp")})
	f.put(&tdObj{kind: kVolume, id: "vol-other-cluster", state: "available", tags: pvcTags("demo-2", "ate-system")})
	f.put(&tdObj{kind: kVolume, id: "vol-other-owner", state: "available", tags: pvcTags("demo", "ate-system", OwnerTagKey, "other.us-west-2")})
	keep := []string{"vol-user-retain", "vol-other-cluster", "vol-other-owner"}

	e, _, logs := tdTestEnv(t, f, false, false)
	rows, err := e.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, r := range rows {
		if r.Kind == kVolume {
			listed[r.ID] = r.Cluster
		}
	}
	for _, v := range []string{"vol-pvc", "vol-auto-pg", "vol-root"} {
		if c, ok := listed[v]; !ok || !c {
			t.Errorf("inventory: %s listed=%v cluster=%v", v, ok, c)
		}
	}
	for _, v := range keep {
		if _, ok := listed[v]; ok {
			t.Errorf("inventory lists %s, which is not this deployment's", v)
		}
	}

	if err := e.teardown(context.Background()); err != nil {
		t.Fatalf("teardown: %v\n%s", err, logs)
	}
	for _, v := range []string{"vol-pvc", "vol-auto-pg"} {
		if countCalls(f.calls, "DeleteVolume "+v) != 1 || f.objs[kVolume+"|"+v] != nil {
			t.Errorf("%s was not deleted once: %v", v, f.calls)
		}
	}
	for _, v := range keep {
		if countCalls(f.calls, "DeleteVolume "+v) != 0 || f.objs[kVolume+"|"+v] == nil {
			t.Errorf("deleted %s", v)
		}
	}
}

// The predicate teardown deletes an un-owned volume on, field by field.
func TestClusterPVCNeedsEveryTag(t *testing.T) {
	e := &tdEnv{n: deriveNames("aws", "111122223333", "us-west-2", "demo")}
	if !e.clusterPVC(pvcTags("demo", "ate-system")) {
		t.Fatal("this cluster's ate-system volume is not matched")
	}
	for name, tags := range map[string]map[string]string{
		"another namespace":     pvcTags("demo", "myapp"),
		"another cluster":       pvcTags("demo-2", "ate-system"),
		"no cluster tag":        {pvcNamespaceTag: "ate-system"},
		"no namespace tag":      {eksClusterNameTag: "demo"},
		"another owner":         pvcTags("demo", "ate-system", OwnerTagKey, "other.us-west-2"),
		"a CloudFormation copy": pvcTags("demo", "ate-system", tdStackKey, "eksctl-demo-cluster"),
	} {
		if e.clusterPVC(tags) {
			t.Errorf("%s: matched %v", name, tags)
		}
	}
}
