package cmd

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// A node group that cannot become ACTIVE by waiting stops the run with the
// installer's way out: fix the cause, then the teardown, never a delete by
// hand. One being deleted is waited out; any other status is no error.
func TestNodegroupFailed(t *testing.T) {
	health := &ekstypes.NodegroupHealth{Issues: []ekstypes.Issue{{Code: ekstypes.NodegroupIssueCodeAsgInstanceLaunchFailures, Message: aws.String("no capacity")}}}
	for _, c := range []struct {
		status ekstypes.NodegroupStatus
		health *ekstypes.NodegroupHealth
		want   []string
	}{
		{ekstypes.NodegroupStatusCreateFailed, health, []string{"node group gvisor of cluster demo is CREATE_FAILED", "AsgInstanceLaunchFailures: no capacity", "fix the cause, then " + recreate}},
		{ekstypes.NodegroupStatusDegraded, nil, []string{"is DEGRADED (EKS reports no health issue)", recreate}},
		{ekstypes.NodegroupStatusDeleteFailed, health, []string{"is DELETE_FAILED", recreate}},
		{ekstypes.NodegroupStatusDeleting, nil, []string{"is being deleted (EKS reports no health issue); wait until EKS has deleted it, and run again"}},
		{ekstypes.NodegroupStatusActive, nil, nil},
		{ekstypes.NodegroupStatusCreating, nil, nil},
	} {
		err := nodegroupFailed("demo", &ekstypes.Nodegroup{NodegroupName: aws.String("gvisor"), Status: c.status, Health: c.health})
		if (err == nil) != (c.want == nil) {
			t.Fatalf("%s: %v", c.status, err)
		}
		if err == nil {
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q lacks %q", c.status, err, w)
			}
		}
		if strings.Contains(err.Error(), "aws eks delete-nodegroup") || strings.Contains(err.Error(), "()") {
			t.Errorf("%s: %q", c.status, err)
		}
	}
}
