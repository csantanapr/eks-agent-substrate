package s3files

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/efs"
)

// fake answers by argv prefix, records stdin, and fails on a shell.
func fake(t *testing.T, answers map[string]string) *[]string {
	var applied []string
	prev, prevWait := plugin.Exec, efs.Wait
	efs.Wait = 0
	plugin.Exec = func(_ context.Context, w io.Writer, c plugin.Cmd) error {
		argv := strings.Join(c.Argv, " ")
		for _, a := range c.Argv {
			if a == "bash" || a == "sh" || a == "-c" {
				return fmt.Errorf("a shell ran: %s", argv)
			}
		}
		if c.Stdin != nil {
			applied = append(applied, string(c.Stdin))
		}
		for p, a := range answers {
			if strings.HasPrefix(argv, p) && c.Stdout != nil {
				io.WriteString(c.Stdout, a)
			}
		}
		return nil
	}
	t.Cleanup(func() { plugin.Exec, efs.Wait = prev, prevWait })
	return &applied
}

func TestFileSystemIDReadsTheDeploymentsBucket(t *testing.T) {
	fake(t, map[string]string{
		"aws sts get-caller-identity": "arn:aws:sts::111122223333:assumed-role/Admin/me",
		"aws s3files list-file-systems --bucket arn:aws:s3:::demo-files-111122223333-us-west-2 ": "fs-0abc",
	})
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc"}
	if id, err := fileSystemID(context.Background(), o); err != nil || id != "fs-0abc" {
		t.Errorf("got %q, %v", id, err)
	}
	// Another bucket's file system is not this deployment's.
	fake(t, map[string]string{
		"aws sts get-caller-identity":                                     "arn:aws:sts::111122223333:assumed-role/Admin/me",
		"aws s3files list-file-systems --bucket arn:aws:s3:::other-files": "fs-0abc",
	})
	if _, err := fileSystemID(context.Background(), o); err == nil || !strings.Contains(err.Error(), "run `setup-aws plugin s3files`") {
		t.Errorf("no file system: %v", err)
	}
}

func TestStorageClassIsS3FilesAccessPoints(t *testing.T) {
	sc, err := efs.Render(efs.StorageTmpl, efs.StorageArgs{Name: StorageClass, Mode: "s3files-ap", FileSystem: "fs-0abc", UID: efs.UID, GID: efs.GID, BasePath: "/ate"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(sc)
	for _, want := range []string{"name: s3files-ate", "provisioningMode: s3files-ap", "fileSystemId: fs-0abc", "provisioner: efs.csi.aws.com", "reclaimPolicy: Delete"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	// Anchored: the template's own comment names the kind.
	if regexp.MustCompile(`(?m)^kind: CSIDriverConfig$`).MatchString(s) {
		t.Error("s3files applies the efs plugin's CSIDriverConfig again")
	}
}
