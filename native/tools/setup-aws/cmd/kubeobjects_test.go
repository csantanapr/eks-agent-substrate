package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The object API against a Kubernetes API: server-side apply with the field
// manager and force, a dry run that stores nothing, get, list with a label
// selector, delete, and what each makes of a 404.
func TestKubeObjects(t *testing.T) {
	kf := newKubeFake(t)
	k := kf.client()
	ctx := context.Background()
	sa := kubeObject{"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata": map[string]any{"name": "karpenter", "namespace": "kube-system", "labels": map[string]any{managedByKey: managedByValue}}}

	if _, err := k.Apply(ctx, sa, true); err != nil {
		t.Fatal(err)
	}
	if kf.get(kindServiceAccount, "kube-system", "karpenter") != nil {
		t.Error("a dry run stored the object")
	}
	got, err := k.Apply(ctx, sa, false)
	if err != nil || got.name() != "karpenter" || got.str("metadata", "uid") == "" {
		t.Fatalf("apply: %v %v", got, err)
	}
	if want := []string{"PATCH /api/v1/namespaces/kube-system/serviceaccounts/karpenter dryRun", "PATCH /api/v1/namespaces/kube-system/serviceaccounts/karpenter"}; strings.Join(kf.calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls %v", kf.calls)
	}
	o, err := k.Get(ctx, kindServiceAccount, "kube-system", "karpenter")
	if err != nil || !ownedObject(o) {
		t.Errorf("get: %v %v", o, err)
	}
	// Absent: an object, or a kind the cluster does not serve yet.
	if o, err := k.Get(ctx, kindServiceAccount, "kube-system", "other"); o != nil || err != nil {
		t.Errorf("get of an absent object: %v %v", o, err)
	}
	if o, err := k.Get(ctx, kindNodePool, "", "gvisor"); o != nil || err != nil {
		t.Errorf("get of an unserved kind: %v %v", o, err)
	}
	if _, err := k.List(ctx, kindNodePool, "", ""); !kubeNotFound(err) {
		t.Errorf("list of an unserved kind: %v", err)
	}
	kf.installCRDs()
	for _, n := range []string{"a", "b"} {
		labels := map[string]any{"karpenter.sh/nodepool": n}
		kf.put(kubeObject{"apiVersion": kindNodeClaim.APIVersion, "kind": kindNodeClaim.Kind, "metadata": map[string]any{"name": "claim-" + n, "labels": labels}})
	}
	kf.put(kubeObject{"apiVersion": kindNodeClaim.APIVersion, "kind": kindNodeClaim.Kind, "metadata": map[string]any{"name": "claim-c"}})
	list, err := k.List(ctx, kindNodeClaim, "", "karpenter.sh/nodepool in (a,b)")
	if err != nil || len(list) != 2 {
		t.Errorf("list by selector: %d items, %v", len(list), err)
	}
	if list, err := k.List(ctx, kindNodeClaim, "", ""); err != nil || len(list) != 3 {
		t.Errorf("list: %d items, %v", len(list), err)
	}
	if err := k.Delete(ctx, kindNodeClaim, "", "claim-a"); err != nil || kf.get(kindNodeClaim, "", "claim-a") != nil {
		t.Errorf("delete: %v", err)
	}
	if err := k.Delete(ctx, kindNodeClaim, "", "claim-a"); err != nil {
		t.Errorf("delete of what is gone: %v", err)
	}
	// A kind outside the table is never sent.
	if _, err := k.Apply(ctx, kubeObject{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "x", "namespace": "y"}}, false); err == nil ||
		!strings.Contains(err.Error(), "does not apply v1 Secret") {
		t.Errorf("apply of a Secret: %v", err)
	}
}

// A refused request is a kubeError with its status and the API's message,
// never the token; 403 is not transient, a 5xx is.
func TestKubeObjectsErrors(t *testing.T) {
	kf := newKubeFake(t)
	k := kf.client()
	ctx := context.Background()
	kf.deny["GET "+kindDeployment.path("kube-system", "karpenter")] = http.StatusForbidden
	_, err := k.Get(ctx, kindDeployment, "kube-system", "karpenter")
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden: denied by the test") || kubeTransient(err) || kubeNotFound(err) {
		t.Errorf("a 403: %v", err)
	}
	if strings.Contains(err.Error(), "k8s-aws-v1") || strings.Contains(err.Error(), "X-Amz-Signature") {
		t.Errorf("the error carries the token: %v", err)
	}
	kf.deny["DELETE "+kindNodePool.path("", "gvisor")] = http.StatusServiceUnavailable
	kf.installCRDs()
	if err := k.Delete(ctx, kindNodePool, "", "gvisor"); err == nil || !kubeTransient(err) {
		t.Errorf("a 503: %v", err)
	}
	// An apply the API refuses says why.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"kind":"Status","message":"NodePool.karpenter.sh \"gvisor\" is invalid: spec.template.spec.expireAfter: Invalid value"}`))
	}))
	defer srv.Close()
	c, err := newKubeClient(kubeTestCluster(srv), fixedPresigner(presignedURL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Apply(ctx, kubeObject{"apiVersion": kindNodePool.APIVersion, "kind": kindNodePool.Kind, "metadata": map[string]any{"name": "gvisor"}}, false)
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "expireAfter: Invalid value") {
		t.Errorf("a 422: %v", err)
	}
	// No answer at all.
	if _, err := unreachableKube(t).List(ctx, kindNodePool, "", ""); err == nil || !kubeTransient(err) {
		t.Errorf("a closed server: %v", err)
	}
}

// Every kind's URL path, core and grouped, namespaced and not.
func TestKubeKindPaths(t *testing.T) {
	for k, want := range map[kubeKind]string{
		kindServiceAccount: "/api/v1/namespaces/kube-system/serviceaccounts/karpenter",
		kindDeployment:     "/apis/apps/v1/namespaces/kube-system/deployments/karpenter",
		kindCRD:            "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/karpenter",
		kindNodePool:       "/apis/karpenter.sh/v1/nodepools/karpenter",
		kindEC2NodeClass:   "/apis/karpenter.k8s.aws/v1/ec2nodeclasses/karpenter",
	} {
		if got := k.path("kube-system", "karpenter"); got != want {
			t.Errorf("%s: %s, want %s", k, got, want)
		}
	}
}
