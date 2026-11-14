package demo

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// Releases publish the worker images but not the counter demo's, so the demo
// builds it, without docker: the counter binary from the cached tree on the
// base image upstream's .ko.yaml names for it,
// pushed to the private ECR repository `setup-aws bootstrap` creates,
// <cluster>/counter. Nothing here runs docker or writes a docker config.

// The package paths ko builds, as upstream's templates name them.
const (
	modulePath = "go.ate.dev/substrate"
	counterPkg = modulePath + "/demos/counter"
)

// counterEntrypoint is where ko puts the binary, and the path the
// ActorTemplate's command runs.
const counterEntrypoint = "/ko-app/counter"

// counterBase is the base image upstream's .ko.yaml gives the counter
// (baseImageOverrides, else defaultBaseImage). It must be pinned by digest,
// so a rebuild of the same release makes the same image.
func counterBase(tree string) (string, error) {
	path := filepath.Join(tree, ".ko.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	n, err := yaml.Parse(string(data))
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	base := ""
	if o := n.Field("baseImageOverrides"); o != nil {
		if v := o.Value.Field(counterPkg); v != nil {
			base = yaml.GetValue(v.Value)
		}
	}
	if base == "" {
		if v := n.Field("defaultBaseImage"); v != nil {
			base = yaml.GetValue(v.Value)
		}
	}
	if !strings.Contains(base, "@sha256:") {
		return "", fmt.Errorf("%s gives %s no base image pinned by digest (found %q)", path, counterPkg, base)
	}
	return base, nil
}

// koRef matches a whole ko:// reference, as ate-setup's images.Prebuilt
// does: a prefix match would turn .../demos/counter-x into the counter image.
// It needs a character after "ko://", so the templates' "the ko:// image
// reference" prose does not match.
var koRef = regexp.MustCompile(`ko://[^\s"',\]}]+`)

// placeholder is a template line ate-setup fills only for options the demo
// does not use (the counter's external volume); with none set it is dropped.
var placeholder = regexp.MustCompile(`^\$\{[A-Z_]+\}$`)

// render is upstream's demo template at path with its ko:// references
// replaced by images (keyed by package path) and its unset placeholder
// lines dropped, the manifest the demo applies with kubectl. A
// reference with no image is an error, not a manifest kubectl would apply.
func render(path string, images map[string]string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !placeholder.MatchString(strings.TrimSpace(l)) {
			kept = append(kept, l)
		}
	}
	var missing []string
	out := koRef.ReplaceAllStringFunc(strings.Join(kept, "\n"), func(ref string) string {
		img, ok := images[strings.TrimPrefix(ref, "ko://")]
		if !ok {
			missing = append(missing, ref)
			return ref
		}
		return img
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: no image for %s", path, strings.Join(missing, ", "))
	}
	return []byte(out), nil
}

// appLayer is the one layer ko adds: the binary at /ko-app/counter, owned by
// root, with fixed timestamps so the same binary gives the same digest.
func appLayer(binary string) (v1.Layer, error) {
	bin, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	epoch := time.Unix(0, 0)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "ko-app/", Mode: 0o755, ModTime: epoch}); err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: strings.TrimPrefix(counterEntrypoint, "/"),
		Mode: 0o755, Size: int64(len(bin)), ModTime: epoch}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(bin); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	return tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
}

// counterImage is the counter image for arch: base's linux/arch image plus
// appLayer, with ko's entrypoint.
func counterImage(ctx context.Context, binary, base, arch string) (v1.Image, error) {
	ref, err := name.ParseReference(base)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: arch}))
	if err != nil {
		return nil, fmt.Errorf("pull base image %s: %w", base, err)
	}
	layer, err := appLayer(binary)
	if err != nil {
		return nil, err
	}
	if img, err = mutate.AppendLayers(img, layer); err != nil {
		return nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cf = cf.DeepCopy()
	cf.Config.Entrypoint = []string{counterEntrypoint}
	return mutate.ConfigFile(img, cf)
}

// ecrAuth is the registry host and push credential of the caller's private
// ECR registry, from ecr:GetAuthorizationToken. The credential stays in
// memory: it is never written to a file, an argv or a log line.
func ecrAuth(ctx context.Context, cfg aws.Config) (string, authn.Authenticator, error) {
	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", nil, fmt.Errorf("ecr:GetAuthorizationToken: %w", err)
	}
	if len(out.AuthorizationData) == 0 {
		return "", nil, errors.New("ecr:GetAuthorizationToken returned no authorization data")
	}
	d := out.AuthorizationData[0]
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(d.AuthorizationToken))
	if err != nil {
		return "", nil, errors.New("ecr:GetAuthorizationToken returned a token that is not base64")
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", nil, errors.New("ecr:GetAuthorizationToken returned a token that is not user:password")
	}
	host := strings.TrimPrefix(aws.ToString(d.ProxyEndpoint), "https://")
	return host, authn.FromConfig(authn.AuthConfig{Username: user, Password: pass}), nil
}

// pushCounter builds the counter image and pushes it to repo:tag in the
// caller's ECR registry. It returns the reference to install it by,
// repo:tag@digest: an ActorTemplate's image must carry a digest.
func pushCounter(ctx context.Context, cfg aws.Config, binary, base, arch, repo, tag string) (string, error) {
	img, err := counterImage(ctx, binary, base, arch)
	if err != nil {
		return "", err
	}
	host, auth, err := ecrAuth(ctx, cfg)
	if err != nil {
		return "", err
	}
	ref, err := name.NewTag(host + "/" + repo + ":" + tag)
	if err != nil {
		return "", err
	}
	if err := remote.Write(ref, img, remote.WithContext(ctx), remote.WithAuth(auth)); err != nil {
		return "", fmt.Errorf("push %s: %w", ref, err)
	}
	d, err := img.Digest()
	if err != nil {
		return "", err
	}
	return ref.String() + "@" + d.String(), nil
}

// remoteDigest pins a public release image: repo/image:tag@digest, read
// anonymously, as ate-setup's Pin does for the worker images.
func remoteDigest(ctx context.Context, tagged string) (string, error) {
	ref, err := name.ParseReference(tagged)
	if err != nil {
		return "", err
	}
	d, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", tagged, err)
	}
	return tagged + "@" + d.Digest.String(), nil
}
