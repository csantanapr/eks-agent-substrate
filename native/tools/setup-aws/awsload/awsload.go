// Package awsload loads the AWS config for a process that can outlive the
// credentials it starts with: a setup-aws step, or the installer's demo step.
//
// Session credentials in the shared credentials file carry no expiry the SDK
// reads, so its own cache keeps them for the life of the process, and the
// SDK does not retry ExpiredToken. A step that ran past the end of its
// session (a create cluster or create rds wait) therefore failed however
// soon the profile was refreshed (issue #5 BUG5). Config's clients instead
// retry a call AWS rejects as signed with expired credentials, reading the
// profile again; while the profile does not yet hold working credentials,
// the retry waits for them and logs that it does.
//
// This is a credential path, so the identity is pinned: the account and
// principal of the first load. Refreshed credentials for anyone else stop
// the step at once; it never switches identity silently. Config returns the
// same config for a region and profile for the life of the process, so every
// client of a setup-aws run or of the installer shares that one pin: a
// profile re-vended for someone else mid-run cannot become a second identity
// for the clients made after it.
package awsload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// Waits for refreshed credentials, replaced in tests.
var (
	pollEvery = 10 * time.Second
	maxWait   = 30 * time.Minute
)

// configs are the configs Config made, by region and profile.
var (
	configsMu sync.Mutex
	configs   = map[[2]string]aws.Config{}
)

// Config loads the config the way the AWS CLI does: region, then profile,
// then the default credential chain. It reads the caller identity once, to
// pin it, and its clients survive the end of the credentials' session, as
// the package comment says. A later call for the same region and profile
// returns the same config, pin and credentials; a failed load is not kept.
func Config(ctx context.Context, region, profile string) (aws.Config, error) {
	configsMu.Lock()
	defer configsMu.Unlock()
	k := [2]string{region, profile}
	if cfg, ok := configs[k]; ok {
		return cfg, nil
	}
	cfg, err := newConfig(ctx, region, profile)
	if err != nil {
		return aws.Config{}, err
	}
	configs[k] = cfg
	return cfg, nil
}

// newConfig is Config's first load for a region and profile.
func newConfig(ctx context.Context, region, profile string) (aws.Config, error) {
	load := func(ctx context.Context) (aws.Config, error) {
		opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
		if profile != "" {
			opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
		}
		return awsconfig.LoadDefaultConfig(ctx, opts...)
	}
	cfg, err := load(ctx)
	if err != nil {
		return aws.Config{}, err
	}
	id, err := whoami(ctx, cfg)
	if err != nil {
		return aws.Config{}, fmt.Errorf("AWS credentials: %w", err)
	}
	p := &reloading{profile: profile, load: load, sdk: cfg.Credentials, pinned: id}
	cache := aws.NewCredentialsCache(p)
	p.invalidate = cache.Invalidate
	cfg.Credentials = cache
	// Setting Retryer replaces the one each client would build from
	// RetryMode, so the mode is honoured here; RetryMaxAttempts the clients
	// still apply on top.
	mode := cfg.RetryMode
	cfg.Retryer = func() aws.Retryer {
		var base aws.RetryerV2 = retry.NewStandard()
		if mode == aws.RetryModeAdaptive {
			base = retry.NewAdaptiveMode()
		}
		return retryer{RetryerV2: base, expired: p.expired}
	}
	return cfg, nil
}

// identity is who credentials sign as: the account, and the caller ARN
// without an assumed role's session name (arn:aws:sts::<account>:assumed-role/Admin).
type identity struct{ account, principal string }

func whoami(ctx context.Context, cfg aws.Config) (identity, error) {
	out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return identity{}, err
	}
	return identity{account: aws.ToString(out.Account), principal: principal(aws.ToString(out.Arn))}, nil
}

// principal drops the session name of an assumed-role ARN; any other ARN (a
// user, a federated user, root) names its principal already.
func principal(arn string) string {
	const marker = ":assumed-role/"
	i := strings.Index(arn, marker)
	if i < 0 {
		return arn
	}
	role, _, _ := strings.Cut(arn[i+len(marker):], "/")
	return arn[:i+len(marker)] + role
}

// expiredCodes are the codes AWS answers a request signed with expired
// credentials with: query and REST-XML services (STS, RDS, IAM, S3),
// JSON services (EKS, ECR, Secrets Manager) and EC2. The same list
// aws-sdk-go v1 refreshed credentials on.
var expiredCodes = map[string]bool{"ExpiredToken": true, "ExpiredTokenException": true, "RequestExpired": true}

func errorCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

func isExpired(err error) bool { return expiredCodes[errorCode(err)] }

// retryer is the SDK's retryer, plus: a call rejected for expired
// credentials is retried, after marking them stale so the retry's signing
// reads the profile again.
type retryer struct {
	aws.RetryerV2
	expired func()
}

func (r retryer) IsErrorRetryable(err error) bool {
	if isExpired(err) {
		r.expired()
		return true
	}
	return r.RetryerV2.IsErrorRetryable(err)
}

// reloading hands out the credentials of the config it was loaded with
// until a call reports them expired. Then it reads the profile again and
// checks what it read with sts:GetCallerIdentity, every pollEvery for up to
// maxWait, until they work and name the pinned identity.
type reloading struct {
	profile    string
	load       func(context.Context) (aws.Config, error)
	invalidate func()
	pinned     identity

	mu    sync.Mutex
	sdk   aws.CredentialsProvider // the SDK's provider of the last good load
	stale atomic.Bool
}

func (p *reloading) expired() {
	p.stale.Store(true)
	p.invalidate()
}

func (p *reloading) Retrieve(ctx context.Context) (aws.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stale.Load() {
		return p.sdk.Retrieve(ctx)
	}
	start, warned := time.Now(), false
	for {
		cfg, err := p.reload(ctx)
		if err == nil {
			p.sdk = cfg.Credentials
			p.stale.Store(false)
			slog.Info("AWS credentials refreshed; continuing", "from", p.name(), "waited", time.Since(start).Round(time.Second))
			return p.sdk.Retrieve(ctx)
		}
		if !errors.Is(err, errNotYet) {
			return aws.Credentials{}, err
		}
		if time.Since(start) >= maxWait {
			return aws.Credentials{}, fmt.Errorf("the AWS credentials of %s expired and were not refreshed within %s: %w", p.name(), maxWait, err)
		}
		if !warned {
			slog.Warn("AWS credentials expired; refresh them and this step continues", "from", p.name(), "waiting_up_to", maxWait)
			warned = true
		}
		select {
		case <-ctx.Done():
			return aws.Credentials{}, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// errNotYet marks a reload worth waiting on: the profile still holds
// expired or not yet valid credentials, or a file being rewritten.
var errNotYet = errors.New("no working credentials yet")

// reload reads the profile again and returns its config when its
// credentials work and are the pinned identity's.
func (p *reloading) reload(ctx context.Context) (aws.Config, error) {
	cfg, err := p.load(ctx)
	if err == nil {
		_, err = cfg.Credentials.Retrieve(ctx)
	}
	if err != nil {
		// The first load worked, so this is a credentials file caught
		// half-written by its refresh.
		return aws.Config{}, fmt.Errorf("%w: %w", errNotYet, err)
	}
	id, err := whoami(ctx, cfg)
	switch {
	case err == nil && id != p.pinned:
		return aws.Config{}, fmt.Errorf("%s: the profile now holds credentials for a different identity; refusing to continue", p.name())
	case err == nil:
		return cfg, nil
	case isExpired(err) || errorCode(err) == "InvalidClientTokenId":
		return aws.Config{}, fmt.Errorf("%w: %w", errNotYet, err)
	}
	return aws.Config{}, err
}

func (p *reloading) name() string {
	if p.profile == "" {
		return "the default credential chain"
	}
	return "profile " + p.profile
}
