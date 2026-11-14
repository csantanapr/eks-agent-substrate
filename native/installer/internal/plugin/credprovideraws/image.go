package credprovideraws

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// The plugin's two images, built here in Go without docker and pushed to
// the plugin's ECR repositories (setup-aws plugin credprovider-aws makes
// them) the way the counter demo's image is (demo/image.go): a static binary
// at ko's path on a base pinned by digest, pushed with an in-memory ECR
// token. Nothing runs docker or writes a docker config.
//
// shortcut: image.go's layer, image and push helpers mirror demo/image.go's
// unexported ones; move both to one shared package if a third plugin needs them.

// The provider's source: the credprovider-aws-secret-manager repository at a
// pinned commit, fetched into the installer's tree cache.
const (
	SourceRepo   = "https://github.com/csantanapr/credprovider-aws-secret-manager.git"
	SourceCommit = "71c74c42e49ea2420592e4e1fea2fa8f728f3427"
	providerPkg  = "./cmd/aws-sm-credential-provider"
	providerBin  = "/ko-app/aws-sm-credential-provider"
	// providerUser is the Dockerfile's USER, the distroless nonroot ids.
	providerUser = "65532:65532"
	probePkg     = "go.ate.dev/substrate/internal/e2e/fixtures/probe"
	probeBin     = "/ko-app/probe"
)

// sourceDir is where the pinned source is cached, beside the Substrate trees.
func sourceDir(treeRoot string) string {
	return filepath.Join(treeRoot, "credprovider-aws-secret-manager-"+SourceCommit[:12])
}

// completeMarker marks a finished fetch, as the Substrate trees' does.
const completeMarker = ".substrate-eks-complete"

// fetchSource clones SourceRepo at SourceCommit into dir unless a finished
// fetch is there: a shallow fetch of the commit into a staging directory,
// checked to be that commit, then renamed into place.
func fetchSource(ctx context.Context, w io.Writer, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err == nil {
		return nil
	}
	fmt.Fprintf(w, "  fetching %s at %s\n", SourceRepo, SourceCommit[:12])
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, argv := range [][]string{
		{"git", "init", "--quiet"},
		{"git", "fetch", "--quiet", "--depth", "1", SourceRepo, SourceCommit},
		{"git", "checkout", "--quiet", "FETCH_HEAD"},
	} {
		if err := plugin.Exec(ctx, w, plugin.Cmd{Dir: stage, Argv: argv}); err != nil {
			return err
		}
	}
	var head bytes.Buffer
	if err := plugin.Exec(ctx, w, plugin.Cmd{Dir: stage, Stdout: &head, Argv: []string{"git", "rev-parse", "HEAD"}}); err != nil {
		return err
	}
	if got := strings.TrimSpace(head.String()); got != SourceCommit {
		return fmt.Errorf("fetched %s, not %s", got, SourceCommit)
	}
	if err := os.WriteFile(filepath.Join(stage, completeMarker), nil, 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(stage, dir)
}

// dockerfileBase is the BASE_IMAGE the provider's Dockerfile pins, which
// must carry a digest.
func dockerfileBase(dockerfile []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(dockerfile))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "ARG BASE_IMAGE="); ok {
			if !strings.Contains(v, "@sha256:") {
				return "", fmt.Errorf("the Dockerfile's BASE_IMAGE %q is not pinned by digest", v)
			}
			return v, nil
		}
	}
	return "", errors.New("the Dockerfile has no ARG BASE_IMAGE=")
}

// koBase is the base image a Substrate tree's .ko.yaml gives pkg
// (baseImageOverrides, else defaultBaseImage), which must carry a digest.
func koBase(koYAML []byte, pkg string) (string, error) {
	n, err := yaml.Parse(string(koYAML))
	if err != nil {
		return "", fmt.Errorf("parse .ko.yaml: %w", err)
	}
	base := ""
	if o := n.Field("baseImageOverrides"); o != nil {
		if v := o.Value.Field(pkg); v != nil {
			base = yaml.GetValue(v.Value)
		}
	}
	if base == "" {
		if v := n.Field("defaultBaseImage"); v != nil {
			base = yaml.GetValue(v.Value)
		}
	}
	if !strings.Contains(base, "@sha256:") {
		return "", fmt.Errorf(".ko.yaml gives %s no base image pinned by digest (found %q)", pkg, base)
	}
	return base, nil
}

// goEnv builds static binaries with the local toolchain. The Substrate tree
// is vendored; the provider's modules come straight from their source
// (proxy.golang.org is unreachable on some build hosts), checked against its
// go.sum.
var (
	treeGoEnv   = []string{"GOTOOLCHAIN=local", "GOFLAGS=-mod=vendor", "CGO_ENABLED=0"}
	sourceGoEnv = []string{"GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly", "GOPROXY=direct", "GOSUMDB=off", "CGO_ENABLED=0"}
)

// image is one image to build and push.
type image struct {
	dir, pkg, entrypoint, user string
	env, ldflags               []string
	base, repo, tag            string
}

// buildPush builds im's binary for arch, puts it on its base and pushes it,
// returning repo:tag@digest.
func (im image) buildPush(ctx context.Context, w io.Writer, ac aws.Config, tmp, arch string) (string, error) {
	bin := filepath.Join(tmp, filepath.Base(im.entrypoint))
	argv := []string{"go", "build", "-trimpath", "-buildvcs=false"}
	if len(im.ldflags) > 0 {
		argv = append(argv, "-ldflags", strings.Join(im.ldflags, " "))
	}
	argv = append(argv, "-o", bin, im.pkg)
	env := append(append([]string{}, im.env...), "GOOS=linux", "GOARCH="+arch)
	fmt.Fprintf(w, "  building %s for linux/%s on %s\n", im.repo, arch, im.base)
	if err := plugin.Exec(ctx, w, plugin.Cmd{Dir: im.dir, Env: env, Argv: argv}); err != nil {
		return "", err
	}
	return pushImage(ctx, ac, bin, im.entrypoint, im.user, im.base, arch, im.repo, im.tag)
}

// appLayer is the one layer ko adds: the binary at entrypoint, owned by root,
// with fixed timestamps so the same binary gives the same digest.
func appLayer(bin []byte, entrypoint string) (v1.Layer, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	epoch := time.Unix(0, 0)
	dir := strings.TrimPrefix(filepath.Dir(entrypoint), "/") + "/"
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755, ModTime: epoch}); err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: strings.TrimPrefix(entrypoint, "/"),
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

// withApp is img plus the binary layer, with the entrypoint and user set.
func withApp(img v1.Image, bin []byte, entrypoint, user string) (v1.Image, error) {
	layer, err := appLayer(bin, entrypoint)
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
	cf.Config.Entrypoint = []string{entrypoint}
	cf.Config.Cmd = nil
	if user != "" {
		cf.Config.User = user
	}
	return mutate.ConfigFile(img, cf)
}

// pushImage is the push seam, replaced in tests.
var pushImage = func(ctx context.Context, ac aws.Config, binary, entrypoint, user, base, arch, repo, tag string) (string, error) {
	bin, err := os.ReadFile(binary)
	if err != nil {
		return "", err
	}
	ref, err := name.ParseReference(base)
	if err != nil {
		return "", err
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(authn.Anonymous),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: arch}))
	if err != nil {
		return "", fmt.Errorf("pull base image %s: %w", base, err)
	}
	if img, err = withApp(img, bin, entrypoint, user); err != nil {
		return "", err
	}
	host, auth, err := ecrAuth(ctx, ac)
	if err != nil {
		return "", err
	}
	dst, err := name.NewTag(host + "/" + repo + ":" + tag)
	if err != nil {
		return "", err
	}
	if err := remote.Write(dst, img, remote.WithContext(ctx), remote.WithAuth(auth)); err != nil {
		return "", fmt.Errorf("push %s: %w", dst, err)
	}
	d, err := img.Digest()
	if err != nil {
		return "", err
	}
	return dst.String() + "@" + d.String(), nil
}

// ecrAuth is the caller's private registry host and push credential, from
// ecr:GetAuthorizationToken. The credential stays in memory: never a file,
// an argv or a log line.
func ecrAuth(ctx context.Context, ac aws.Config) (string, authn.Authenticator, error) {
	out, err := ecr.NewFromConfig(ac).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
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
	return strings.TrimPrefix(aws.ToString(d.ProxyEndpoint), "https://"), authn.FromConfig(authn.AuthConfig{Username: user, Password: pass}), nil
}

// pinRelease is a release image's tagged reference with its digest appended,
// read anonymously. Replaced in tests.
var pinRelease = func(ctx context.Context, tagged string) (string, error) {
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

// images builds and pushes the provider and probe images for arch.
func images(ctx context.Context, w io.Writer, o plugin.Options, ac aws.Config, arch string) (provider, probe string, err error) {
	src := sourceDir(o.TreeRoot)
	if err := fetchSource(ctx, w, src); err != nil {
		return "", "", fmt.Errorf("fetch the provider source: %w", err)
	}
	df, err := os.ReadFile(filepath.Join(src, "Dockerfile"))
	if err != nil {
		return "", "", err
	}
	pbase, err := dockerfileBase(df)
	if err != nil {
		return "", "", err
	}
	tree := o.Tree
	ko, err := os.ReadFile(filepath.Join(tree, ".ko.yaml"))
	if err != nil {
		return "", "", fmt.Errorf("no substrate %s tree at %s; turn on Substrate first: %w", o.Tag, tree, err)
	}
	qbase, err := koBase(ko, probePkg)
	if err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp("", "substrate-eks-credprovider-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	version := SourceCommit[:12]
	provider, err = image{dir: src, pkg: providerPkg, entrypoint: providerBin, user: providerUser, env: sourceGoEnv,
		ldflags: []string{"-s", "-w", "-X", "main.version=" + version}, base: pbase,
		repo: o.Cluster + "/aws-sm-credential-provider", tag: version}.buildPush(ctx, w, ac, tmp, arch)
	if err != nil {
		return "", "", fmt.Errorf("provider image: %w", err)
	}
	probe, err = image{dir: tree, pkg: "./internal/e2e/fixtures/probe", entrypoint: probeBin, env: treeGoEnv, base: qbase,
		repo: o.Cluster + "/credprovider-probe", tag: o.Tag}.buildPush(ctx, w, ac, tmp, arch)
	if err != nil {
		return "", "", fmt.Errorf("probe image: %w", err)
	}
	return provider, probe, nil
}
