package controlplane

// The StorageClass prerequisite: encrypted gp3 EBS volumes that carry this
// deployment's tags. ate-setup v1.0.0 deploys the bundled PostgreSQL and
// waits for it before it applies the rest of its render, so the class cannot
// ride in the overlay (GKE's dynamic-rwo is built in). The deploy renders it
// here and applies it first, from kubectl's stdin.
//
//   https://docs.aws.amazon.com/eks/latest/userguide/create-storage-class.html
//   https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/master/docs/tagging.md

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"reflect"
	"sort"
	"strings"
)

const (
	// AutoModeStorageClass keeps the name earlier installs gave the bundled
	// PostgreSQL claim: a StatefulSet cannot change its claim templates.
	AutoModeStorageClass = "substrate-eks-auto-ebs"
	// StandardStorageClass is the class on a cluster without Auto Mode.
	StandardStorageClass = "substrate-eks-ebs"

	// Auto Mode provisions EBS through its own driver, and its node
	// autoscaler launches no node for a claim of another provisioner. A
	// standard cluster uses the aws-ebs-csi-driver add-on, which
	// `setup-aws create cluster` installs.
	autoModeProvisioner = "ebs.csi.eks.amazonaws.com"
	ebsCSIProvisioner   = "ebs.csi.aws.com"

	// DefaultTags is TAGS when it is unset, as in state.NewSetup and setup-aws.
	DefaultTags = "project=agent-substrate auto-delete=no"
)

// NoVolumeTagsLine is printed when Auto Mode volumes get no custom tags.
const NoVolumeTagsLine = "This installer did not create the cluster, so its role may not let EKS Auto Mode put custom tags on volumes: the bundled PostgreSQL volume gets no owner tag, and teardown finds it by its eks:eks-cluster-name tag and namespace ate-system"

// StorageClassName is the class the deploy applies, and the bundled
// PostgreSQL's (--ateapi-postgres-storage-class).
func StorageClassName(autoMode bool) string {
	if autoMode {
		return AutoModeStorageClass
	}
	return StandardStorageClass
}

func provisioner(autoMode bool) string {
	if autoMode {
		return autoModeProvisioner
	}
	return ebsCSIProvisioner
}

// volumeTags are the owner tag and TAGS (space-separated key=value), the
// tags setup-aws puts on every resource it creates (AC2.1).
func volumeTags(cluster, region, tags string) (map[string]string, error) {
	m := map[string]string{ownerTagKey: cluster + "." + region}
	for _, kv := range strings.Fields(tags) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("TAGS: %q is not key=value", kv)
		}
		m[k] = v
	}
	return m, nil
}

type (
	topologyRequirement struct {
		Key    string   `json:"key"`
		Values []string `json:"values"`
	}
	topologyTerm struct {
		MatchLabelExpressions []topologyRequirement `json:"matchLabelExpressions"`
	}
	objectMeta struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations,omitempty"`
	}
	storageClass struct {
		APIVersion           string            `json:"apiVersion"`
		Kind                 string            `json:"kind"`
		Metadata             objectMeta        `json:"metadata"`
		Provisioner          string            `json:"provisioner"`
		Parameters           map[string]string `json:"parameters"`
		ReclaimPolicy        string            `json:"reclaimPolicy"`
		VolumeBindingMode    string            `json:"volumeBindingMode"`
		AllowVolumeExpansion bool              `json:"allowVolumeExpansion"`
		AllowedTopologies    []topologyTerm    `json:"allowedTopologies,omitempty"`
	}
)

// renderStorageClass is the class: gp3, encrypted with the account's
// default EBS key, created when a pod first uses the claim, deleted with
// it, and one tagSpecification_N per tag, in key order. isDefault makes it
// the class of a claim that names none (R1: a cluster this installer made).
func renderStorageClass(autoMode, isDefault bool, tags map[string]string) storageClass {
	sc := storageClass{
		APIVersion: "storage.k8s.io/v1", Kind: "StorageClass",
		Metadata:    objectMeta{Name: StorageClassName(autoMode)},
		Provisioner: provisioner(autoMode),
		Parameters:  map[string]string{"type": "gp3", "encrypted": "true"},
		// Delete, so a removed install leaves no volume behind.
		ReclaimPolicy: "Delete", VolumeBindingMode: "WaitForFirstConsumer", AllowVolumeExpansion: true,
	}
	if isDefault {
		sc.Metadata.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	}
	if autoMode {
		// The cluster also has managed node groups, where Auto Mode's driver
		// cannot attach a volume; this keeps the claim's pod on Auto Mode nodes.
		sc.AllowedTopologies = []topologyTerm{{MatchLabelExpressions: []topologyRequirement{
			{Key: "eks.amazonaws.com/compute-type", Values: []string{"auto"}}}}}
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		sc.Parameters[fmt.Sprintf("tagSpecification_%d", i+1)] = k + "=" + tags[k]
	}
	return sc
}

// sameImmutable reports whether two classes agree on every field Kubernetes
// will not update in place.
func sameImmutable(a, b storageClass) bool {
	return a.Provisioner == b.Provisioner && maps.Equal(a.Parameters, b.Parameters) &&
		a.ReclaimPolicy == b.ReclaimPolicy && a.VolumeBindingMode == b.VolumeBindingMode &&
		reflect.DeepEqual(a.AllowedTopologies, b.AllowedTopologies)
}

// applyStorageClass applies the class with this deployment's tags where the
// provisioner can add them. The aws-ebs-csi-driver add-on's policy allows any
// tag. Auto Mode's managed policy allows only its own tag keys, so the tags go
// on only when the cluster role has the statement `setup-aws create cluster`
// adds to it: on a cluster this installer created. A class that differs in a
// field Kubernetes cannot update is replaced; the volumes it already made
// keep their settings.
func applyStorageClass(ctx context.Context, w io.Writer, o Options, kc string) error {
	env := []string{"KUBECONFIG=" + kc}
	tags := o.volumeTags
	if o.AutoMode && !o.clusterOwned {
		fmt.Fprintln(w, NoVolumeTagsLine)
		tags = nil
	}
	if !o.AutoMode && o.db() == DatabaseBundled {
		// Without the driver the claim stays Pending, and ate-setup times out
		// waiting for PostgreSQL without saying why.
		if err := runCommand(ctx, w, command{Env: env, Stdout: io.Discard,
			Argv: []string{"kubectl", "get", "csidriver", ebsCSIProvisioner}}); err != nil {
			return fmt.Errorf("the bundled PostgreSQL needs the EBS CSI driver %s, the aws-ebs-csi-driver add-on: %w", ebsCSIProvisioner, err)
		}
	}
	want := renderStorageClass(o.AutoMode, o.clusterOwned, tags)
	fmt.Fprintf(w, "%sStorageClass %s (%s, encrypted gp3, %d volume tags)\n", PrereqLine, want.Metadata.Name, want.Provisioner, len(tags))
	var cur strings.Builder
	if err := runCommand(ctx, w, command{Env: env, Stdout: &cur,
		Argv: []string{"kubectl", "get", "storageclass", want.Metadata.Name, "-o", "json", "--ignore-not-found"}}); err != nil {
		return err
	}
	verb := []string{"apply", "-f", "-"}
	if s := strings.TrimSpace(cur.String()); s != "" {
		var have storageClass
		if err := json.Unmarshal([]byte(s), &have); err != nil {
			return fmt.Errorf("read StorageClass %s: %w", want.Metadata.Name, err)
		}
		if !sameImmutable(have, want) {
			fmt.Fprintln(w, "StorageClass "+want.Metadata.Name+" exists with other settings; replacing it")
			verb = []string{"replace", "--force", "-f", "-"}
		}
	}
	b, err := json.Marshal(want)
	if err != nil {
		return err
	}
	return runCommand(ctx, w, command{Env: env, Stdin: b, Argv: append([]string{"kubectl"}, verb...)})
}
