package s3files

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/efs"
)

const (
	ID           = "s3files"
	StorageClass = "s3files-ate"
	Template     = "counter-s3files"
)

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "Amazon S3 Files volumes for actors",
		Summary: "Each actor gets its own S3 Files access point as its data volume, its files synced to a private, versioned S3 bucket " +
			"(ExternalVolumes preview). Uses the efs plugin's CSI driver. Adds counter-s3files to the counter demo.",
		Order:    31,
		Minutes:  10,
		Requires: []string{efs.ID},
		SetupAWS: true,
		Needs:    plugin.Needs{Preview: []string{"ExternalVolumes"}},
		// S3 Files bills stored data and requests, not hours.
		HourlyUSD: 0.01,
		Deploy:    deploy,
		Verify:    verify,
		Remove:    remove,
	})
}

// fileSystemID is the S3 file system on this deployment's bucket
// <cluster>-files-<account>-<region> (`setup-aws plugin s3files` made both).
func fileSystemID(ctx context.Context, o plugin.Options) (string, error) {
	arn, err := efs.AWS(ctx, o, "sts", "get-caller-identity", "--query", "Arn", "--output", "text")
	if err != nil {
		return "", err
	}
	p := strings.Split(arn, ":")
	if len(p) < 5 {
		return "", fmt.Errorf("unexpected caller identity")
	}
	bucket := fmt.Sprintf("arn:%s:s3:::%s-files-%s-%s", p[1], o.Cluster, p[4], o.Region)
	id, err := efs.AWS(ctx, o, "s3files", "list-file-systems", "--bucket", bucket,
		"--query", "fileSystems[?status!='deleted'].fileSystemId | [0]", "--output", "text")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(id, "fs-") {
		return "", fmt.Errorf("no S3 file system on bucket %s-files-<account>-%s: run `setup-aws plugin s3files`", o.Cluster, o.Region)
	}
	return id, nil
}

func deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	// The node plugin mounts S3 Files with its own Pod Identity role, which
	// `setup-aws plugin s3files` associated after the add-on started.
	if err := efs.EnsurePodIdentity(ctx, w, o, "kube-system", "daemonset/efs-csi-node"); err != nil {
		return err
	}
	fs, err := fileSystemID(ctx, o)
	if err != nil {
		return err
	}
	sc, err := efs.Render(efs.StorageTmpl, efs.StorageArgs{Name: StorageClass, Mode: "s3files-ap", FileSystem: fs, UID: efs.UID, GID: efs.GID, BasePath: "/ate"})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Applying StorageClass %s (S3 file system %s)\n", StorageClass, fs)
	if err := efs.ApplyStorageClass(ctx, w, o, StorageClass, fs, efs.UID, sc); err != nil {
		return err
	}
	k, err := efs.BuildAte(ctx, w, o)
	if err != nil {
		return err
	}
	return efs.CreateTemplates(ctx, w, k, Template, StorageClass)
}

func storage(o plugin.Options) efs.Storage {
	return efs.Storage{Name: ID, Template: Template, AccessPoints: func(ctx context.Context) ([]string, error) {
		fs, err := fileSystemID(ctx, o)
		if err != nil {
			return nil, err
		}
		out, err := efs.AWS(ctx, o, "s3files", "list-access-points", "--file-system-id", fs, "--query", "accessPoints[].accessPointId", "--output", "json")
		if err != nil {
			return nil, err
		}
		var ids []string
		return ids, json.Unmarshal([]byte(out), &ids)
	}}
}

// verify is demos/efs/verify.sh with STORAGE=s3files on every counter class:
// about 2 minutes per class.
func verify(ctx context.Context, w io.Writer, o plugin.Options) error {
	k, err := efs.BuildAte(ctx, w, o)
	if err != nil {
		return err
	}
	for _, c := range efs.Classes {
		if err := efs.RoundTrip(ctx, w, o, k, storage(o), c, false); err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	return nil
}

// remove deletes the counter-s3files actors (their access points go with
// them) and templates, and the StorageClass. `setup-aws plugin s3files
// --delete` then deletes the file system, the bucket and the roles.
func remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	if k, err := efs.BuildAte(ctx, w, o); err != nil {
		fmt.Fprintf(w, "WARNING: %v; leaving the %s actors to the file system's deletion\n", err, Template)
	} else if err := efs.DeleteTemplates(ctx, w, k, Template); err != nil {
		return err
	}
	return efs.Run(ctx, w, o, "delete", "storageclass", StorageClass, "--ignore-not-found")
}
