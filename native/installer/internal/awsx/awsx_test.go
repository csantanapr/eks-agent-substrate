package awsx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteKubeconfigIsPrivateAndCarriesTheProfile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k", "c.yaml")
	cl := Cluster{Name: "demo", Endpoint: "https://example.eks.amazonaws.com", CA: "Zm9v"}
	if err := WriteKubeconfig(p, cl, "us-west-2", "eks-sandbox"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	for _, want := range []string{`"get-token"`, `"--profile"`, `"eks-sandbox"`, "current-context: demo", "name: demo"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("kubeconfig lacks %s", want)
		}
	}
	if strings.Contains(string(b), "token:") {
		t.Error("kubeconfig stores a token")
	}
}

func TestWriteKubeconfigRefusesMissingCA(t *testing.T) {
	if err := WriteKubeconfig(filepath.Join(t.TempDir(), "c"), Cluster{Name: "d", Endpoint: "https://x"}, "r", ""); err == nil {
		t.Fatal("wrote a kubeconfig without a CA")
	}
}

func TestSupportedAndBadge(t *testing.T) {
	for _, tc := range []struct {
		c    Cluster
		ok   bool
		want string
	}{
		{Cluster{Version: "1.37", Status: "ACTIVE", AutoMode: true}, true, "ready · Auto Mode"},
		{Cluster{Version: "1.38", Status: "ACTIVE"}, true, "ready"},
		{Cluster{Version: "1.36", Status: "ACTIVE"}, false, "needs 1.37+"},
		{Cluster{Version: "1.37", Status: "CREATING"}, false, "creating"},
	} {
		if got := tc.c.Supported(); got != tc.ok {
			t.Errorf("%+v Supported = %v", tc.c, got)
		}
		if got := tc.c.Badge(); got != tc.want {
			t.Errorf("%+v Badge = %q, want %q", tc.c, got, tc.want)
		}
	}
}

// Only a cluster carrying both of setup-aws's tags for this deployment counts
// as created by the installer.
func TestCreatedByInstaller(t *testing.T) {
	both := map[string]string{CreatedByTagKey: CreatedByValue, OwnerTagKey: "c.us-west-2"}
	for _, tc := range []struct {
		tags   map[string]string
		region string
		want   bool
	}{
		{both, "us-west-2", true},
		{both, "us-east-1", false}, // another deployment's name
		{map[string]string{OwnerTagKey: "c.us-west-2"}, "us-west-2", false},
		{map[string]string{CreatedByTagKey: CreatedByValue}, "us-west-2", false},
		{map[string]string{CreatedByTagKey: "eksctl", OwnerTagKey: "c.us-west-2"}, "us-west-2", false},
		{nil, "us-west-2", false},
	} {
		if got := (Cluster{Name: "c", Tags: tc.tags}).CreatedByInstaller(tc.region); got != tc.want {
			t.Errorf("%v in %s: %v, want %v", tc.tags, tc.region, got, tc.want)
		}
	}
}

func TestValidClusterName(t *testing.T) {
	for name, ok := range map[string]bool{
		"agent-substrate": true, "a1": true, "sub-native": true,
		"a": false, "1abc": false, "Abc": false, "ab_c": false, "ab-": false,
		"a234567890123456789012345678": true, "a2345678901234567890123456789": false,
	} {
		if err := ValidClusterName(name); (err == nil) != ok {
			t.Errorf("ValidClusterName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
}
