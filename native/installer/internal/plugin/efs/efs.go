package efs

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Names the plugin's objects use (demos/efs/deploy.sh).
const (
	ID           = "efs"
	StorageClass = "efs-ate"
	Template     = "counter-efs"
	csiNS        = "kube-system"
	controller   = "efs-csi-controller-ate"
	// The Envoy release in front of the driver's socket, pinned by digest.
	envoyImage = "envoyproxy/envoy:v1.39.5@sha256:8fc594a5d85eb76dca24155cfb9208882406873932b20cf33649a0a611194c43"
	// UID and GID own each actor's access point. Match the user the actor
	// runs as: actors have no CAP_DAC_OVERRIDE, so even root cannot write a
	// 0700 directory another uid owns. The counter runs as root.
	//
	// shortcut: fixed at config.env's EFS_UID/EFS_GID defaults; make them
	// options when an actor image runs as another user.
	UID, GID = "0", "0"
)

var (
	//go:embed controller.yaml
	controllerYAML string
	//go:embed storage.yaml
	storageYAML string

	controllerTmpl = template.Must(template.New("controller").Parse(controllerYAML))
	// StorageTmpl renders a CSIDriverConfig (DriverConfig) and a
	// StorageClass for per-actor volumes; s3files renders its class with it.
	StorageTmpl = template.Must(template.New("storage").Parse(storageYAML))
)

// StorageArgs are StorageTmpl's fields.
type StorageArgs struct {
	DriverConfig                               bool
	Name, Mode, FileSystem, UID, GID, BasePath string
}

// Render executes t with data.
func Render(t *template.Template, data any) ([]byte, error) {
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "Amazon EFS volumes for actors",
		Summary: "Each actor gets its own Amazon EFS access point as its data volume, mounted on whichever node it resumes on " +
			"(ExternalVolumes preview). Adds counter-efs to the counter demo.",
		Order:    30,
		Minutes:  15,
		SetupAWS: true,
		Needs:    plugin.Needs{Preview: []string{"ExternalVolumes"}},
		// EFS bills stored data and throughput, not hours: a few test
		// volumes cost cents a month.
		HourlyUSD: 0.01,
		Deploy:    deploy,
		Verify:    verify,
		Remove:    remove,
	})
}

func deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	fmt.Fprintln(w, "Waiting for the aws-efs-csi-driver add-on's node plugin and controller")
	for _, obj := range []string{"daemonset/efs-csi-node", "deployment/efs-csi-controller"} {
		if err := Run(ctx, w, o, "-n", csiNS, "rollout", "status", obj, "--timeout=5m"); err != nil {
			return err
		}
	}
	if err := EnsurePodIdentity(ctx, w, o, csiNS, "deployment/efs-csi-controller"); err != nil {
		return err
	}
	img, err := Out(ctx, o, "-n", csiNS, "get", "deployment", "efs-csi-controller", "-o",
		`jsonpath={.spec.template.spec.containers[?(@.name=="efs-plugin")].image}`)
	if err != nil {
		return err
	}
	if img = strings.TrimSpace(img); img == "" {
		return fmt.Errorf("the add-on's efs-csi-controller has no efs-plugin container")
	}
	fmt.Fprintln(w, "Driver image: "+img[strings.LastIndex(img, "/")+1:])

	tags, err := OwnerTagsArg(o)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(controllerYAML + img + tags))
	m, err := Render(controllerTmpl, map[string]string{"DriverImage": img, "EnvoyImage": envoyImage, "Tags": tags, "ConfigSHA": hex.EncodeToString(sum[:])})
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "Applying Substrate's EFS CSI controller (ClusterIP, mTLS)")
	if err := Apply(ctx, w, o, m); err != nil {
		return err
	}
	if err := Run(ctx, w, o, "-n", csiNS, "rollout", "status", "deployment/"+controller, "--timeout=5m"); err != nil {
		return err
	}
	if err := EnsurePodIdentity(ctx, w, o, csiNS, "deployment/"+controller); err != nil {
		return err
	}

	fs, err := FileSystemID(ctx, o)
	if err != nil {
		return err
	}
	sc, err := Render(StorageTmpl, StorageArgs{DriverConfig: true, Name: StorageClass, Mode: "efs-ap", FileSystem: fs, UID: UID, GID: GID, BasePath: "/ate"})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Applying the CSIDriverConfig and StorageClass %s (file system %s)\n", StorageClass, fs)
	if err := ApplyStorageClass(ctx, w, o, StorageClass, fs, UID, sc); err != nil {
		return err
	}
	k, err := BuildAte(ctx, w, o)
	if err != nil {
		return err
	}
	return CreateTemplates(ctx, w, k, Template, StorageClass)
}

// EFSStorage is the efs check's Storage: access points listed by EFS.
func EFSStorage(o plugin.Options) Storage {
	return Storage{Name: "efs", Template: Template, AccessPoints: func(ctx context.Context) ([]string, error) {
		fs, err := FileSystemID(ctx, o)
		if err != nil {
			return nil, err
		}
		out, err := AWS(ctx, o, "efs", "describe-access-points", "--file-system-id", fs, "--query", "AccessPoints[].AccessPointId", "--output", "json")
		if err != nil {
			return nil, err
		}
		var ids []string
		return ids, json.Unmarshal([]byte(out), &ids)
	}}
}

// verify is verify.sh on every counter class and verify-move.sh on gVisor:
// about 2 minutes per class, plus up to 10 for the move, which waits for the
// suspend's snapshot to reach S3 (5 to 6 minutes on EKS).
func verify(ctx context.Context, w io.Writer, o plugin.Options) error {
	k, err := BuildAte(ctx, w, o)
	if err != nil {
		return err
	}
	s := EFSStorage(o)
	for _, c := range Classes {
		if err := RoundTrip(ctx, w, o, k, s, c, c.Name == "gvisor"); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	return nil
}

// remove is demos/efs/clean.sh's Kubernetes half: the counter-efs actors
// (their access points go with them) and templates, the StorageClass, the
// CSIDriverConfig and Substrate's controller. `setup-aws plugin efs
// --delete` then deletes the add-on, the role and the file system.
func remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	if k, err := BuildAte(ctx, w, o); err != nil {
		// Without kubectl-ate the actors stay; their access points go with
		// the file system.
		fmt.Fprintf(w, "WARNING: %v; leaving the %s actors to the file system's deletion\n", err, Template)
	} else if err := DeleteTemplates(ctx, w, k, Template); err != nil {
		return err
	}
	if err := Run(ctx, w, o, "delete", "storageclass", StorageClass, "--ignore-not-found"); err != nil {
		return err
	}
	if crd, err := Out(ctx, o, "get", "crd", "csidriverconfigs.ate.dev", "--ignore-not-found", "-o", "name"); err != nil {
		return err
	} else if strings.TrimSpace(crd) != "" {
		if err := Run(ctx, w, o, "delete", "csidriverconfig", "efs.csi.aws.com", "--ignore-not-found"); err != nil {
			return err
		}
	}
	return Run(ctx, w, o, "-n", csiNS, "delete", "deployment/"+controller, "service/"+controller, "configmap/"+controller+"-envoy", "--ignore-not-found")
}
