package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const configEnv = `# Settings for every script.

# --- Cluster ---
# 2-28 characters of a-z, 0-9 and '-'.
CLUSTER_NAME=agent-substrate
AWS_REGION=us-west-2 # the Region
AWS_PROFILE=
KUBERNETES_VERSION=1.37
AUTO_MODE=on
SYSTEM_INSTANCE_TYPE=m8i.xlarge
GVISOR_INSTANCE_TYPE=m8i.2xlarge
MICROVM_INSTANCE_TYPE=m8i.2xlarge
SYSTEM_NODES=2
GVISOR_NODES=2
MICROVM_NODES=2
# Space-separated key=value tags.
TAGS="project=agent-substrate auto-delete=no"
EKS_PUBLIC_ACCESS_CIDRS='0.0.0.0/0'
SUBSTRATE_TAG=v1.0.0
SUBSTRATE_IMAGE_REPO=ghcr.io/agent-substrate/substrate
export DB_STORAGE_GB=20
`

// checkout makes an eks-talk checkout: config.env, config.local.env when
// local is not empty, and native/tools/setup-aws/go.mod.
func checkout(t *testing.T, local string) string {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "native", "tools", "setup-aws"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "native", "tools", "setup-aws", "go.mod"), []byte("module x\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "config.env"), []byte(configEnv), 0o644))
	if local != "" {
		must(os.WriteFile(filepath.Join(dir, "config.local.env"), []byte(local), 0o644))
	}
	return dir
}

func TestParseConfigAcceptsWhatTheScriptsUse(t *testing.T) {
	f, err := ParseConfig(strings.NewReader(configEnv))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"AWS_REGION": "us-west-2", "AWS_PROFILE": "", "TAGS": "project=agent-substrate auto-delete=no",
		"EKS_PUBLIC_ACCESS_CIDRS": "0.0.0.0/0", "DB_STORAGE_GB": "20"} {
		if got, ok := f.Get(k); !ok || got != want {
			t.Errorf("%s = %q, %v; want %q", k, got, ok, want)
		}
	}
	if d := f.Doc("CLUSTER_NAME"); d != "2-28 characters of a-z, 0-9 and '-'." {
		t.Errorf("doc %q (a section heading is not the key's doc)", d)
	}
}

// Anything that is not a plain assignment is refused, so a script's file is
// never half-read.
func TestParseConfigRefusesShell(t *testing.T) {
	for _, bad := range []string{`echo hi`, `X=$(id)`, `X="$HOME"`, "X=`id`", `X=a b`, `X="open`, `X='a' b`, `f() { :; }`} {
		if _, err := ParseConfig(strings.NewReader(bad + "\n")); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The environment wins over config.local.env, which wins over config.env;
// outside a checkout only the environment is read.
func TestConfigLookupOrder(t *testing.T) {
	dir := checkout(t, "SUBSTRATE_TAG=v1.0.1\nAWS_PROFILE=team\n")
	env := map[string]string{"AWS_PROFILE": "mine"}
	c, err := FindConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, filepath.Join(dir, "native", "installer"))
	if err != nil || c.Dir != dir {
		t.Fatalf("FindConfig: %q %v", c.Dir, err)
	}
	for k, want := range map[string]string{"AWS_PROFILE": "mine", "SUBSTRATE_TAG": "v1.0.1", "KUBERNETES_VERSION": "1.37"} {
		if v, _ := c.Lookup(k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	none, err := FindConfig(func(string) (string, bool) { return "", false }, t.TempDir())
	if err != nil || none.Dir != "" || none.LocalPath() != "" {
		t.Errorf("outside a checkout: %+v %v", none, err)
	}
	if _, ok := none.Lookup("KUBERNETES_VERSION"); ok {
		t.Error("read a config.env that is not there")
	}
}

// config.local.env gets the changed keys and the deployment's own, keeps the
// keys the wizard does not manage, is 0600, and a different earlier file is
// backed up.
func TestRenderAndWriteLocal(t *testing.T) {
	dir := checkout(t, "WHATSAPP_PEER=+15550100\nSUBSTRATE_TAG=v1.0.1\n")
	c, err := LoadConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSetupFrom(c.Lookup)
	s.ClusterName, s.Region, s.Profile = "sub-demo", "us-west-2", "eks-sandbox"
	s.NewClusterConfig.GVisorNodes = "3"
	text, err := c.RenderLocal(ConfigValues(s))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CLUSTER_NAME=sub-demo\n", "AWS_REGION=us-west-2\n", "AWS_PROFILE=eks-sandbox\n", "GVISOR_NODES=3\n",
		"SUBSTRATE_TAG=v1.0.1\n", "# Kept from the previous config.local.env\nWHATSAPP_PEER=+15550100\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("config.local.env lacks %q:\n%s", want, text)
		}
	}
	for _, unchanged := range []string{"SYSTEM_NODES=", "TAGS=", "KUBERNETES_VERSION="} {
		if strings.Contains(text, unchanged) {
			t.Errorf("wrote %s, which equals config.env:\n%s", unchanged, text)
		}
	}
	s.Tags = `team=a "b"`
	if text, _ := c.RenderLocal(ConfigValues(s)); !strings.Contains(text, `TAGS="team=a \"b\""`) {
		t.Errorf("TAGS not quoted for bash:\n%s", text)
	}
	backup := filepath.Join(t.TempDir(), "backups", "config.local.env.1")
	if err := c.WriteLocal(text, backup); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(c.LocalPath()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("config.local.env mode %v (%v)", st.Mode().Perm(), err)
	}
	if b, _ := os.ReadFile(backup); !strings.Contains(string(b), "WHATSAPP_PEER") {
		t.Errorf("no backup of the earlier file: %q", b)
	}
	if b, _ := os.ReadFile(c.LocalPath()); string(b) != text {
		t.Errorf("written %q", b)
	}
	if err := (Config{}).WriteLocal(text, backup); err == nil {
		t.Error("wrote config.local.env outside a checkout")
	}
	if _, err := c.RenderLocal(map[string]string{"TAGS": "a=b\nX=1"}); err == nil {
		t.Error("a line break in a value was accepted")
	}
}

// Each check passes config.env's defaults and refuses a bad value, and
// names the key it refuses.
func TestValidateConfigBothWays(t *testing.T) {
	c, _ := ParseConfig(strings.NewReader(configEnv))
	good := map[string]string{}
	for _, e := range c.Entries {
		good[e.Key] = e.Value
	}
	if bad := ValidateConfig(good); len(bad) != 0 {
		t.Fatalf("config.env refused: %v", bad)
	}
	for key, value := range map[string]string{
		"KUBERNETES_VERSION": "1.36", "AUTO_MODE": "yes", "SYSTEM_INSTANCE_TYPE": "big", "GVISOR_NODES": "0",
		"MICROVM_NODES": "100", "TAGS": "a=b c", "EKS_PUBLIC_ACCESS_CIDRS": "", "SUBSTRATE_TAG": "latest",
		"SUBSTRATE_IMAGE_REPO": "Not A Repo",
	} {
		v := map[string]string{}
		for k, x := range good {
			v[k] = x
		}
		v[key] = value
		if bad := ValidateConfig(v); len(bad) != 1 || bad[key] == "" {
			t.Errorf("%s=%q: findings %v, want one for %s", key, value, bad, key)
		}
	}
	good["KUBERNETES_VERSION"], good["AUTO_MODE"], good["EKS_PUBLIC_ACCESS_CIDRS"] = "1.38", "", "203.0.113.0/24 198.51.100.0/24"
	if bad := ValidateConfig(good); len(bad) != 0 {
		t.Errorf("valid edits refused: %v", bad)
	}
	// Both ways for setup-aws's default: "auto" alone is accepted; "auto"
	// mixed with a range, or another word, is refused.
	for value, ok := range map[string]bool{"auto": true, "auto 203.0.113.0/24": false, "Auto": false, "any": false} {
		good["EKS_PUBLIC_ACCESS_CIDRS"] = value
		if bad := ValidateConfig(good); (len(bad) == 0) != ok {
			t.Errorf("EKS_PUBLIC_ACCESS_CIDRS=%q: findings %v, want accepted=%v", value, bad, ok)
		}
	}
}
