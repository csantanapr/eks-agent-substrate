package cmd

import (
	"context"
	"strings"
	"testing"
)

func testEnv(t *testing.T, f *fakeAWS, plan bool) *env {
	t.Helper()
	cfg = Config{Region: "us-west-2", ClusterName: "demo", Tags: "project=agent-substrate auto-delete=no",
		ECRRepositories: "counter", MinKubernetesMinor: 37, Plan: plan}
	newClients = func(context.Context, Config) (*Clients, error) { return f.clients(), nil }
	e, err := newEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func count(writes []string, prefix string) int {
	n := 0
	for _, w := range writes {
		if strings.HasPrefix(w, prefix) {
			n++
		}
	}
	return n
}

func TestBootstrapFreshAccountCreatesEverything(t *testing.T) {
	f := newFake()
	if err := testEnv(t, f, false).bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	for prefix, want := range map[string]int{
		"CreateBucket ": 2, "PutPublicAccessBlock ": 2, "PutBucketPolicy ": 2,
		"CreateRole ": 2, "PutRolePolicy ": 2, "CreatePodIdentityAssociation ": 2, "CreateRepository ": 1,
	} {
		if got := count(f.writes, prefix); got != want {
			t.Errorf("%s: got %d, want %d (writes %v)", prefix, got, want, f.writes)
		}
	}
	if got := f.buckets["demo-snap-111122223333-us-west-2"][OwnerTagKey]; got != "demo.us-west-2" {
		t.Errorf("snapshot bucket owner tag = %q", got)
	}
	if got := f.roles["demo-us-west-2-atelet"]["auto-delete"]; got != "no" {
		t.Errorf("atelet role auto-delete tag = %q", got)
	}
	if !f.repos["demo/counter"] {
		t.Error("ECR repository demo/counter missing")
	}
	// Least privilege: atelet may never delete snapshots; ate-api-server may.
	if strings.Contains(f.policies["demo-us-west-2-atelet"], "s3:DeleteObject") {
		t.Error("atelet policy grants s3:DeleteObject")
	}
	if !strings.Contains(f.policies["demo-us-west-2-ate-api-server"], "s3:DeleteObject") {
		t.Error("ate-api-server policy lacks s3:DeleteObject")
	}
}

func TestBootstrapSecondRunCreatesNothing(t *testing.T) {
	f := newFake()
	if err := testEnv(t, f, false).bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.writes = nil
	if err := testEnv(t, f, false).bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, w := range f.writes {
		if strings.HasPrefix(w, "Create") {
			t.Errorf("second run created something: %s", w)
		}
	}
	// It still reconciles settings (both directions: writes happen, creates don't).
	if count(f.writes, "PutBucketPolicy ") != 2 {
		t.Errorf("second run did not reconcile bucket policies: %v", f.writes)
	}
}

func TestPlanWritesNothing(t *testing.T) {
	f := newFake()
	if err := testEnv(t, f, true).bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Errorf("--plan made %d writes: %v", len(f.writes), f.writes)
	}
}

func TestForeignBucketIsRefused(t *testing.T) {
	f := newFake()
	f.buckets["demo-snap-111122223333-us-west-2"] = map[string]string{OwnerTagKey: "someone-else"}
	err := testEnv(t, f, false).bootstrap(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not owned by") {
		t.Fatalf("want a not-owned error, got %v", err)
	}
	if count(f.writes, "PutBucket") != 0 {
		t.Errorf("changed a bucket it does not own: %v", f.writes)
	}
}

func TestDeniedReadStopsInsteadOfCreating(t *testing.T) {
	f := newFake()
	f.denyHead = true
	err := testEnv(t, f, false).bootstrap(context.Background())
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("want AccessDenied, got %v", err)
	}
	if count(f.writes, "CreateBucket ") != 0 {
		t.Error("a denied HeadBucket was treated as not found")
	}
}

func TestAssociationToAnotherRoleIsRepointed(t *testing.T) {
	f := newFake()
	f.assocs["ate-system/atelet"] = "arn:aws:iam::111122223333:role/old"
	if err := testEnv(t, f, false).bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := "arn:aws:iam::111122223333:role/demo-us-west-2-atelet"; f.assocs["ate-system/atelet"] != want {
		t.Errorf("association = %q, want %q", f.assocs["ate-system/atelet"], want)
	}
	if count(f.writes, "UpdatePodIdentityAssociation ") != 1 {
		t.Errorf("want one update: %v", f.writes)
	}
}

func TestClusterChecks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		version  string
		autoMode bool
		addons   []string
		wantErr  string
	}{
		{"auto mode 1.37", "1.37", true, nil, ""},
		{"too old", "1.36", true, nil, "older than"},
		{"standard with agent", "1.37", false, []string{"vpc-cni", "eks-pod-identity-agent"}, ""},
		{"standard without agent", "1.37", false, []string{"vpc-cni"}, "eks-pod-identity-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.version, f.autoMode, f.addons = tc.version, tc.autoMode, tc.addons
			_, err := testEnv(t, f, false).checkCluster(context.Background())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
