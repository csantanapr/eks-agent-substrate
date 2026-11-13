package demo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
)

// newReal is env backed by os/exec, net/http and the AWS SDK.
func newReal(o Options) *env {
	var (
		once   sync.Once
		cfg    aws.Config
		cfgErr error
	)
	awsConfig := func(ctx context.Context) (aws.Config, error) {
		once.Do(func() {
			opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(o.Region)}
			if o.Profile != "" {
				opts = append(opts, awsconfig.WithSharedConfigProfile(o.Profile))
			}
			cfg, cfgErr = awsconfig.LoadDefaultConfig(ctx, opts...)
		})
		return cfg, cfgErr
	}
	return &env{
		run:    runStreaming,
		output: runOutput,
		start:  startProcess,
		get:    httpGet,
		objects: func(ctx context.Context, bucket, prefix string) (int, error) {
			c, err := awsConfig(ctx)
			if err != nil {
				return 0, err
			}
			// shortcut: one page (up to 1000 keys) is enough to show the
			// snapshot exists; paginate if the count itself ever matters.
			out, err := s3.NewFromConfig(c).ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
			if err != nil {
				return 0, err
			}
			return int(aws.ToInt32(out.KeyCount)), nil
		},
		account: func(ctx context.Context) (string, error) {
			c, err := awsConfig(ctx)
			if err != nil {
				return "", err
			}
			id, err := sts.NewFromConfig(c).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return "", err
			}
			return aws.ToString(id.Account), nil
		},
		kubeconfig: func(ctx context.Context) (string, error) { return ensureKubeconfig(ctx, o) },
		freePort:   freePort,
		kubectlAte: kubectlAtePath(o.Tag),
		interval:   2 * time.Second,
	}
}

// ensureKubeconfig returns the installer's kubeconfig for the cluster, which
// the control plane step writes, writing it first if it is gone.
func ensureKubeconfig(ctx context.Context, o Options) (string, error) {
	// Always rewritten, as Deploy and Delete do: a kubeconfig left by an
	// earlier run carries that run's --profile, and reusing it would run
	// kubectl-ate as one identity and the S3 check as another (review F8).
	kc := awsx.KubeconfigPath(o.Cluster, o.Region)
	c, err := awsx.New(ctx, o.Profile, o.Region)
	if err != nil {
		return "", err
	}
	cl, err := c.Describe(ctx, o.Cluster)
	if err != nil {
		return "", err
	}
	return kc, awsx.WriteKubeconfig(kc, cl, o.Region, o.Profile)
}

// kubectlAtePath is where Verify builds kubectl-ate, in the installer's cache
// beside its trees, one binary per release.
func kubectlAtePath(tag string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "substrate-eks", "bin", "kubectl-ate-"+tag)
}

func command(ctx context.Context, dir string, extra []string, argv ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Dir = dir
	c.Env = append(os.Environ(), extra...)
	return c
}

func label(argv []string) string { return strings.Join(argv[:min(len(argv), 4)], " ") }

func runStreaming(ctx context.Context, w io.Writer, dir string, extra []string, argv ...string) error {
	c := command(ctx, dir, extra, argv...)
	c.Stdout, c.Stderr = w, w
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", label(argv), err)
	}
	return nil
}

func runOutput(ctx context.Context, extra []string, argv ...string) ([]byte, error) {
	c := command(ctx, "", extra, argv...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", label(argv), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// startProcess starts argv and returns a stop that kills it and waits.
func startProcess(ctx context.Context, w io.Writer, extra []string, argv ...string) (func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	c := command(ctx, "", extra, argv...)
	c.Stdout, c.Stderr = w, w
	if err := c.Start(); err != nil {
		cancel()
		return nil, err
	}
	return func() { cancel(); _ = c.Wait() }, nil
}

func httpGet(ctx context.Context, url string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

// freePort asks the kernel for a free localhost port. Another process can
// take it before the proxy binds it; the proxy then fails and says so.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	a, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("no TCP port")
	}
	return a.Port, nil
}
