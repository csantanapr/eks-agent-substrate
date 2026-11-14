package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
)

var (
	// fakeUpdateStatus is what DescribeUpdate reports once an update has
	// been read InProgress once; fakeUpdateDone is set when it reports
	// Successful.
	fakeUpdateStatus = ekstypes.UpdateStatusSuccessful
	fakeUpdateReads  int
	fakeUpdateDone   bool
	// testProbe, when set, is the endpoint probe of runs from clusterTestRun.
	testProbe func(*ekstypes.Cluster) error
)

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
	fakeUpdateReads, fakeUpdateDone = 0, false
	return &eks.UpdateClusterConfigOutput{Update: &ekstypes.Update{Id: aws.String(f.id("update")), Status: ekstypes.UpdateStatusInProgress}}, f.end(w)
}

func (e *fcEKS) DescribeUpdate(_ context.Context, in *eks.DescribeUpdateInput, _ ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error) {
	if !e.clusterExists(in.Name) || !strings.HasPrefix(aws.ToString(in.UpdateId), "update-") {
		return nil, apiErr("ResourceNotFoundException")
	}
	fakeUpdateReads++
	u := &ekstypes.Update{Id: in.UpdateId, Status: ekstypes.UpdateStatusInProgress}
	if fakeUpdateReads > 1 {
		u.Status = fakeUpdateStatus
	}
	switch u.Status {
	case ekstypes.UpdateStatusSuccessful:
		fakeUpdateDone = true
	case ekstypes.UpdateStatusFailed:
		u.Errors = []ekstypes.ErrorDetail{{ErrorCode: ekstypes.ErrorCodeUnknown, ErrorMessage: aws.String("boom")}}
	}
	return &eks.DescribeUpdateOutput{Update: u}, nil
}

// checkIPServer answers like checkip.amazonaws.com: answer(n) is the status
// and body of the n-th request (from 1). It counts requests and new
// connections.
type checkIP struct{ requests, conns atomic.Int32 }

func checkIPServer(t *testing.T, answer func(n int) (int, string)) *checkIP {
	t.Helper()
	c := &checkIP{}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		status, body := answer(int(c.requests.Add(1)))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	s.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			c.conns.Add(1)
		}
	}
	s.Start()
	t.Cleanup(s.Close)
	old := checkIPURL
	checkIPURL = s.URL
	t.Cleanup(func() { checkIPURL = old })
	return c
}

func always(status int, body string) func(int) (int, string) {
	return func(int) (int, string) { return status, body }
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

// bothFixes: every refusal names both ways out.
func bothFixes(t *testing.T, err error) {
	t.Helper()
	for _, want := range []string{"set EKS_PUBLIC_ACCESS_CIDRS to your network's egress ranges", "0.0.0.0/0 (IAM still authenticates every call)"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestPublicAccessDefaultIsAuto(t *testing.T) {
	if _, set := os.LookupEnv("EKS_PUBLIC_ACCESS_CIDRS"); set {
		t.Skip("EKS_PUBLIC_ACCESS_CIDRS is set in this environment")
	}
	if got := createClusterCmd.Flags().Lookup("public-access-cidrs").DefValue; got != publicAccessAuto {
		t.Errorf("default %q, want %q", got, publicAccessAuto)
	}
}

// auto, five equal answers on five new connections: the new cluster's
// endpoint accepts this machine's address, a /32.
func TestPublicAccessAutoAcceptsFiveEqualSamples(t *testing.T) {
	ip := checkIPServer(t, always(http.StatusOK, "198.51.100.7\n"))
	f := newClusterFake()
	if err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.clusterIn.ResourcesVpcConfig.PublicAccessCidrs; !slices.Equal(got, []string{"198.51.100.7/32"}) {
		t.Errorf("public access CIDRs %v, want [198.51.100.7/32]", got)
	}
	if r, c := ip.requests.Load(), ip.conns.Load(); r != 5 || c != 5 {
		t.Errorf("%d lookups on %d connections, want 5 on 5", r, c)
	}
	if u := updates(f.writes); u != nil {
		t.Errorf("a fresh cluster got an endpoint update: %v", u)
	}
}

// One answer out of five differs (a NAT with two addresses): auto is
// refused before any write.
func TestPublicAccessAutoRefusesDifferingSamples(t *testing.T) {
	checkIPServer(t, func(n int) (int, string) {
		if n == 3 {
			return http.StatusOK, "198.51.100.8"
		}
		return http.StatusOK, "198.51.100.7"
	})
	f := newClusterFake()
	err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "from 2 addresses") {
		t.Fatalf("want a refusal naming 2 addresses, got %v", err)
	}
	bothFixes(t, err)
	if len(f.writes) != 0 || f.clusterIn != nil {
		t.Errorf("writes %v after differing lookups", f.writes)
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
			checkIPServer(t, always(tc.status, tc.body))
			f := newClusterFake()
			err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background())
			bothFixes(t, err)
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
		bothFixes(t, err)
		if len(f.writes) != 0 {
			t.Errorf("writes %v after a failed lookup", f.writes)
		}
	})
}

// auto on a cluster that exists never changes its ranges, and needs no
// lookup: a lookup that would fail does not stop the rerun.
func TestPublicAccessAutoLeavesAnExistingClusterAlone(t *testing.T) {
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, cidrs := range [][]string{{"203.0.113.0/24", "198.51.100.7/32"}, {"0.0.0.0/0"}, {"192.0.2.9/32"}} {
		ip := checkIPServer(t, always(http.StatusInternalServerError, ""))
		publicCluster(f, cidrs...)
		f.writes = nil
		if err := clusterTestRun(t, f, false, autoCIDRs).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if u := updates(f.writes); u != nil {
			t.Errorf("auto changed an existing cluster's ranges %v: %v", cidrs, u)
		}
		if got := f.cluster.ResourcesVpcConfig.PublicAccessCidrs; !slices.Equal(got, cidrs) {
			t.Errorf("ranges %v, want %v kept", got, cidrs)
		}
		if n := ip.requests.Load(); n != 0 {
			t.Errorf("%d lookups for an existing cluster", n)
		}
	}
}

// An explicit EKS_PUBLIC_ACCESS_CIDRS replaces an existing cluster's ranges
// and keeps both endpoint switches; the same list again changes nothing.
func TestPublicAccessExplicitValueReconcilesTheRanges(t *testing.T) {
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
	if vc := f.cluster.ResourcesVpcConfig; !vc.EndpointPrivateAccess || !vc.EndpointPublicAccess {
		t.Errorf("endpoint switches changed: public %v, private %v", vc.EndpointPublicAccess, vc.EndpointPrivateAccess)
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
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	publicCluster(f, "203.0.113.0/24")
	f.writes = nil
	explicit := func(c *clusterConfig) { c.PublicAccessCIDRs = "198.51.100.7/32" }
	probed := 0
	testProbe = func(*ekstypes.Cluster) error { probed++; return nil }
	t.Cleanup(func() { testProbe = nil })
	r := clusterTestRun(t, f, true, explicit)
	if err := r.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 || !slices.ContainsFunc(r.changes, func(c string) bool { return strings.Contains(c, "let 198.51.100.7/32 reach") }) {
		t.Errorf("plan: writes %v, changes %v", f.writes, r.changes)
	}
	if probed != 0 {
		t.Error("a plan probed the endpoint")
	}

	fakeUpdateStatus = ekstypes.UpdateStatusFailed
	t.Cleanup(func() { fakeUpdateStatus = ekstypes.UpdateStatusSuccessful })
	err := clusterTestRun(t, f, false, explicit).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is Failed: Unknown: boom") {
		t.Fatalf("want the failed update's error, got %v", err)
	}
	if probed != 0 {
		t.Error("probed after a failed update")
	}
}

// The probe runs on a new cluster once it is ACTIVE, and after an access
// update only once the update reports Successful; a probe failure fails
// the run with both fixes.
func TestProbeRunsAfterActiveAndASuccessfulUpdate(t *testing.T) {
	var seen []string
	fakeUpdateDone = false
	testProbe = func(c *ekstypes.Cluster) error {
		seen = append(seen, string(c.Status)+" updateDone="+map[bool]string{true: "yes", false: "no"}[fakeUpdateDone])
		return nil
	}
	t.Cleanup(func() { testProbe = nil })
	f := newClusterFake()
	if err := clusterTestRun(t, f, false).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"ACTIVE updateDone=no"}) {
		t.Errorf("new cluster: probes %v, want one on ACTIVE", seen)
	}

	publicCluster(f, "203.0.113.0/24")
	seen = nil
	if err := clusterTestRun(t, f, false, func(c *clusterConfig) { c.PublicAccessCIDRs = "192.0.2.0/24" }).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"ACTIVE updateDone=yes"}) || fakeUpdateReads < 2 {
		t.Errorf("after an update: probes %v (update read %d times), want one after Successful", seen, fakeUpdateReads)
	}

	testProbe = func(*ekstypes.Cluster) error { return errProbe }
	err := clusterTestRun(t, f, false, func(c *clusterConfig) { c.PublicAccessCIDRs = "192.0.2.0/24" }).run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot reach the Kubernetes API of cluster demo") || !strings.Contains(err.Error(), "which accepts 192.0.2.0/24") {
		t.Fatalf("want an unreachable-API error, got %v", err)
	}
	bothFixes(t, err)
}

var errProbe = &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}

// probeServer is a TLS API server whose n-th new connection is closed at
// once when fail(n) is true. It returns the endpoint, its CA as EKS
// reports it (base64 PEM) and the new-connection count.
func probeServer(t *testing.T, fail func(n int) bool) (string, string, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // no credentials: still an answer
	}))
	s.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st == http.StateNew && fail(int(conns.Add(1))) {
			c.Close()
		}
	}
	s.StartTLS()
	t.Cleanup(s.Close)
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
	return s.URL, ca, &conns
}

// otherCA is a self-signed certificate that signed nothing, as EKS
// reports a CA.
func otherCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestProbeAPI(t *testing.T) {
	pollEvery = 0
	oldWarm := probeWarmup
	probeWarmup = 200 * time.Millisecond
	t.Cleanup(func() { probeWarmup = oldWarm })

	t.Run("all six connections answer", func(t *testing.T) {
		url, ca, conns := probeServer(t, func(int) bool { return false })
		if err := probeAPI(context.Background(), url, ca); err != nil {
			t.Fatal(err)
		}
		if n := conns.Load(); n != 6 {
			t.Errorf("%d connections, want 6 new ones", n)
		}
	})
	t.Run("one of six fails", func(t *testing.T) {
		url, ca, _ := probeServer(t, func(n int) bool { return n == 4 })
		err := probeAPI(context.Background(), url, ca)
		if err == nil || !strings.Contains(err.Error(), "connection 4 of 6 failed") {
			t.Fatalf("want connection 4 of 6 to fail, got %v", err)
		}
	})
	t.Run("nothing answers", func(t *testing.T) {
		url, ca, conns := probeServer(t, func(int) bool { return true })
		err := probeAPI(context.Background(), url, ca)
		if err == nil || !strings.Contains(err.Error(), "no answer in") || conns.Load() < 2 {
			t.Fatalf("want no answer after retries, got %v (%d connections)", err, conns.Load())
		}
	})
	t.Run("another CA", func(t *testing.T) {
		url, _, _ := probeServer(t, func(int) bool { return false })
		if err := probeAPI(context.Background(), url, otherCA(t)); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("reached a server whose certificate the cluster CA did not sign: %v", err)
		}
		if err := probeAPI(context.Background(), url, ""); err == nil || !strings.Contains(err.Error(), "no endpoint or certificate authority") {
			t.Fatalf("want a missing-CA error, got %v", err)
		}
	})
}
