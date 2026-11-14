package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// fixedNameAssocs are the Pod Identity associations, by fixed namespace and
// ServiceAccount, of the efs, s3files, db-rotation and llm-gateway plugins.
// With the efs plugin's EKS add-on, they are the cluster objects with fixed
// names the plugins make.
var fixedNameAssocs = map[string][2]string{ // association id -> namespace, ServiceAccount
	"a-efs-ctrl": {"kube-system", "efs-csi-controller-sa"},
	"a-efs-node": {"kube-system", "efs-csi-node-sa"},
	"a-eso":      {"external-secrets", "external-secrets"},
	"a-llm":      {"llm-gateway", "llm-gateway"},
}

// A cluster the user brought, which already runs the aws-efs-csi-driver
// add-on and Pod Identity associations of their own for those
// ServiceAccounts (tagged auto-delete=no only, as the user made them): a
// teardown --keep-cluster with every such plugin compiled in passes, leaves
// them as they are, and inventory never lists them. The same objects with
// this deployment's owner tag are listed and deleted.
func TestTeardownKeepClusterLeavesTheUsersFixedNameObjects(t *testing.T) {
	defs := map[string]pluginDef{}
	for _, id := range []string{"efs", "s3files", dbRotationID, "llm-gateway"} {
		defs[id] = pluginDefs[id]
	}
	for _, mine := range []bool{false, true} {
		name := map[bool]string{false: "the user's own", true: "this deployment's"}[mine]
		t.Run(name, func(t *testing.T) {
			tags := map[string]string{"auto-delete": "no"}
			if mine {
				tags = own()
			}
			td := fullDeployment()
			e, _, logs := tdTestEnv(t, td, false, true)
			for id, d := range defs {
				pluginDefs[id] = d
			}
			for id, nsa := range fixedNameAssocs {
				td.put(&tdObj{kind: "assoc", id: id, ns: nsa[0], sa: nsa[1], role: roleARN("someone-elses"), tags: tags})
			}
			f := newFakeNFS()
			f.addons[efsAddon] = &ekstypes.Addon{AddonName: aws.String(efsAddon), Status: ekstypes.AddonStatusActive, Tags: tags}
			installNFS(t, f, newFakeDB())
			cf, rot := &fakeCFN{calls: &td.calls}, &fakeRotTD{calls: &td.calls, rotation: map[string]bool{}}
			prev := rotationClientsFor
			rotationClientsFor = func(context.Context, Config) (*rotClients, error) {
				return &rotClients{CFN: cf, EC2: rot, SM: rot}, nil
			}
			t.Cleanup(func() { rotationClientsFor = prev })

			rows, err := e.inventory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"EKS add-on " + efsAddon}
			for id, nsa := range fixedNameAssocs {
				want = append(want, "Pod Identity association "+nsa[0]+"/"+nsa[1]+" ("+id+")")
			}
			for _, w := range want {
				if hasRow(rows, w) != mine {
					t.Errorf("inventory lists %q: %v, want %v", w, hasRow(rows, w), mine)
				}
			}

			if err := e.teardown(context.Background()); err != nil {
				t.Fatalf("teardown --keep-cluster: %v\n%s", err, logs)
			}
			deleted := strings.Contains(strings.Join(f.writes, ","), "DeleteAddon "+efsAddon)
			if _, left := f.addons[efsAddon]; deleted != mine || left == mine {
				t.Errorf("add-on: deleted %v, still there %v; want deleted only when it is this deployment's (%v)", deleted, left, f.writes)
			}
			for id := range fixedNameAssocs {
				deleted := countCalls(td.calls, "DeletePodIdentityAssociation "+id) > 0
				if left := td.get("assoc", id) != nil; deleted != mine || left == mine {
					t.Errorf("association %s: deleted %v, still there %v; want deleted only when it is this deployment's", id, deleted, left)
				}
			}
		})
	}
}
