package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

// fakeUpdateStatus is what DescribeUpdate reports for every update.
var fakeUpdateStatus = ekstypes.UpdateStatusSuccessful

// UpdateClusterConfig applies the endpoint settings the way EKS does: a
// switch left out takes its documented default (public on, private off),
// so a request that omits the private switch turns private access off.
func (e *fcEKS) UpdateClusterConfig(_ context.Context, in *eks.UpdateClusterConfigInput, _ ...func(*eks.Options)) (*eks.UpdateClusterConfigOutput, error) {
	f := e.f()
	if !e.clusterExists(in.Name) {
		return nil, apiErr("ResourceNotFoundException")
	}
	rv := in.ResourcesVpcConfig
	w := "UpdateClusterConfig " + aws.ToString(in.Name) + " " + strings.Join(rv.PublicAccessCidrs, " ")
	if err := f.begin(w); err != nil {
		return nil, err
	}
	vc := *f.cluster.ResourcesVpcConfig
	vc.PublicAccessCidrs = slices.Clone(rv.PublicAccessCidrs)
	vc.EndpointPublicAccess = rv.EndpointPublicAccess == nil || *rv.EndpointPublicAccess
	vc.EndpointPrivateAccess = aws.ToBool(rv.EndpointPrivateAccess)
	f.cluster.ResourcesVpcConfig = &vc
	return &eks.UpdateClusterConfigOutput{Update: &ekstypes.Update{Id: aws.String(f.id("update")), Status: ekstypes.UpdateStatusInProgress}}, f.end(w)
}

func (e *fcEKS) DescribeUpdate(_ context.Context, in *eks.DescribeUpdateInput, _ ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error) {
	if !e.clusterExists(in.Name) || !strings.HasPrefix(aws.ToString(in.UpdateId), "update-") {
		return nil, apiErr("ResourceNotFoundException")
	}
	u := &ekstypes.Update{Id: in.UpdateId, Status: fakeUpdateStatus}
	if fakeUpdateStatus == ekstypes.UpdateStatusFailed {
		u.Errors = []ekstypes.ErrorDetail{{ErrorCode: ekstypes.ErrorCodeUnknown, ErrorMessage: aws.String("boom")}}
	}
	return &eks.DescribeUpdateOutput{Update: u}, nil
}

// checkIPServer answers like checkip.amazonaws.com, with body and status.
func checkIPServer(t *testing.T, status int, body string) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	old := checkIPURL
	checkIPURL = s.URL
	t.Cleanup(func() { checkIPURL = old })
}

func autoCIDRs(c *clusterConfig) { c.PublicAccessCIDRs = publicAccessAuto }

// publicCluster makes the fake's cluster report both endpoint switches on,
// as a real one created by setup-aws does, with these ranges.
func publicCluster(f *clusterFake, cidrs ...string) {
	vc := *f.cluster.ResourcesVpcConfig
	vc.EndpointPublicAccess, vc.EndpointPrivateAccess, vc.PublicAccessCidrs = true, true, cidrs
	f.cluster.ResourcesVpcConfig = &vc
}

func updates(writes []string) []string {
	var out []string
	for _, w := range writes {
		if strings.HasPrefix(w, "UpdateClusterConfig ") {
			out = append(out, w)
		}
	}
	return out
}

func TestPublicAccessDefaultIsAuto(t *testing.T) {
	if _, set := os.LookupEnv("EKS_PUBLIC_ACCESS_CIDRS"); set {
		t.Skip("EKS_PUBLIC_ACCESS_CIDRS is set in this environment")
	}
	if got := createClusterCmd.Flags().Lookup("public-access-cidrs").DefValue; got != publicAccessAuto {
		t.Errorf("default %q, want %q", got, publicAccessAuto)
	}
}

// auto: the new cluster's endpoint accepts this machine's address, a /32.
func TestPublicAccessAutoResolvesToACallerSlash32(t *testing.T) {
	checkIPServer(t, http.StatusOK, "198.51.100.7\n")
	f := newClusterFake()
	r := clusterTestRun(t, f, false, autoCIDRs)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.clusterIn.ResourcesVpcConfig.PublicAccessCidrs; !slices.Equal(got, []string{"198.51.100.7/32"}) {
		t.Errorf("public access CIDRs %v, want [198.51.100.7/32]", got)
	}
	if u := updates(f.writes); u != nil {
		t.Errorf("a fresh cluster got an endpoint update: %v", u)
	}
}

// A failed or odd lookup stops the run before any write, and never falls
// back to 0.0.0.0/0.
func TestPublicAccessAutoRefusesABadLookup(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"server error":    {http.StatusInternalServerError, "198.51.100.7"},
		"not an address":  {http.StatusOK, "<html>hello</html>"},
		"private address": {http.StatusOK, "10.1.2.3"},
		"IPv6 address":    {http.StatusOK, "2001:db8::7"},
		"loopback":        {http.StatusOK, "127.0.0.1"},
	} {
		t.Run(name, func(t *testing.T) {
			checkIPServer(t, tc.status, tc.body)
			f := newClusterFake()
			err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "set EKS_PUBLIC_ACCESS_CIDRS") {
				t.Fatalf("want a lookup error naming EKS_PUBLIC_ACCESS_CIDRS, got %v", err)
			}
			if len(f.writes) != 0 || f.clusterIn != nil {
				t.Errorf("writes %v after a failed lookup", f.writes)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		s := httptest.NewServer(http.NotFoundHandler())
		old := checkIPURL
		checkIPURL = s.URL
		s.Close() // nothing listens there now
		t.Cleanup(func() { checkIPURL = old })
		f := newClusterFake()
		err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "set EKS_PUBLIC_ACCESS_CIDRS") || len(f.writes) != 0 {
			t.Fatalf("want a lookup error and no writes, got %v (writes %v)", err, f.writes)
		}
	})
}

// A rerun from a new address adds it next to the old one and keeps both
// endpoint switches as they are; one from a covered address changes nothing.
func TestPublicAccessAutoRerunFromANewAddressAddsIt(t *testing.T) {
	checkIPServer(t, http.StatusOK, "198.51.100.7")
	f := newClusterFake()
	if err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	publicCluster(f, "198.51.100.7/32")

	checkIPServer(t, http.StatusOK, "203.0.113.9")
	f.writes = nil
	if err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u := updates(f.writes); !slices.Equal(u, []string{"UpdateClusterConfig demo 198.51.100.7/32 203.0.113.9/32"}) {
		t.Errorf("updates %v", u)
	}
	if vc := f.cluster.ResourcesVpcConfig; !vc.EndpointPrivateAccess || !vc.EndpointPublicAccess {
		t.Errorf("endpoint switches changed: public %v, private %v", vc.EndpointPublicAccess, vc.EndpointPrivateAccess)
	}

	for _, cidrs := range [][]string{{"203.0.113.9/32", "198.51.100.7/32"}, {"0.0.0.0/0"}, {"203.0.113.0/24"}} {
		publicCluster(f, cidrs...)
		f.writes = nil
		if err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if u := updates(f.writes); u != nil {
			t.Errorf("cluster allows %v, which covers this machine, yet got %v", cidrs, u)
		}
	}
}

// An explicit EKS_PUBLIC_ACCESS_CIDRS replaces the cluster's ranges; the
// same list again changes nothing.
func TestPublicAccessExplicitValueReplacesTheRanges(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	publicCluster(f, "203.0.113.0/24", "198.51.100.7/32")
	f.writes = nil
	explicit := func(c *clusterConfig) { c.PublicAccessCIDRs = "192.0.2.0/24" }
	if err := clusterTestRun(t, f, false, explicit).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u := updates(f.writes); !slices.Equal(u, []string{"UpdateClusterConfig demo 192.0.2.0/24"}) {
		t.Errorf("updates %v", u)
	}
	f.writes = nil
	if err := clusterTestRun(t, f, false, explicit).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u := updates(f.writes); u != nil {
		t.Errorf("the same list again: updates %v", u)
	}
}

// --plan reports the update and makes none; a failed update fails the run.
func TestPublicAccessUpdatePlanAndFailure(t *testing.T) {
	checkIPServer(t, http.StatusOK, "198.51.100.7")
	f := newClusterFake()
	if err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	publicCluster(f, "203.0.113.0/24")
	f.writes = nil
	r := clusterTestRun(t, f, true, autoCIDRs)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || !slices.ContainsFunc(r.changes, func(c string) bool { return strings.Contains(c, "198.51.100.7/32 reach") }) {
		t.Errorf("plan: writes %v, changes %v", f.writes, r.changes)
	}

	fakeUpdateStatus = ekstypes.UpdateStatusFailed
	t.Cleanup(func() { fakeUpdateStatus = ekstypes.UpdateStatusSuccessful })
	err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is Failed: Unknown: boom") {
		t.Fatalf("want the failed update's error, got %v", err)
	}
}
