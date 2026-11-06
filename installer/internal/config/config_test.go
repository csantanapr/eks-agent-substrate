package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `# Settings for every script.
#
# --- Cluster ---
# 2-28 characters of a-z.
CLUSTER_NAME=agent-substrate
AWS_REGION=us-west-2
# Empty: the SDK chain.
AWS_PROFILE=
# Tags, space separated.
TAGS="project=agent-substrate team=x"
SINGLE='a b'
TRAIL=word # a comment
export EXPORTED=yes
#   aws ec2 describe-instance-types --filters x
MICROVM_INSTANCE_TYPE=m8i.2xlarge
`

func TestParse(t *testing.T) {
	f, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CLUSTER_NAME": "agent-substrate", "AWS_REGION": "us-west-2", "AWS_PROFILE": "",
		"TAGS": "project=agent-substrate team=x", "SINGLE": "a b", "TRAIL": "word", "EXPORTED": "yes",
		"MICROVM_INSTANCE_TYPE": "m8i.2xlarge",
	}
	for k, v := range want {
		got, ok := f.Get(k)
		if !ok || got != v {
			t.Errorf("%s = %q (%v), want %q", k, got, ok, v)
		}
	}
	if d := f.Doc("CLUSTER_NAME"); d != "2-28 characters of a-z." {
		t.Errorf("doc of CLUSTER_NAME = %q: the --- section line must not be part of it", d)
	}
	if d := f.Doc("AWS_REGION"); d != "" {
		t.Errorf("doc of AWS_REGION = %q, want none", d)
	}
	if d := f.Doc("MICROVM_INSTANCE_TYPE"); d != "  aws ec2 describe-instance-types --filters x" {
		t.Errorf("an indented comment line keeps its indent, got %q", d)
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{
		"echo hi\n",
		"A=$(date)\n",
		`A="$HOME"` + "\n",
		"A=two words\n",
		`A="open` + "\n",
		`A="x" y` + "\n",
	} {
		if _, err := Parse(strings.NewReader(s)); err == nil {
			t.Errorf("Parse(%q) took it; want an error", s)
		}
	}
}

// The eks-talk config.env this installer ships next to parses, with the
// keys the installer asks about.
func TestParseRepoConfig(t *testing.T) {
	f, err := ParseFile(filepath.Join("..", "..", "..", "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Entries) == 0 {
		t.Skip("no config.env next to installer/")
	}
	for _, k := range Managed {
		if k == "AWS_PROFILE" {
			continue
		}
		if v, ok := f.Get(k); !ok || v == "" {
			t.Errorf("config.env has no %s", k)
		}
	}
}

func TestRender(t *testing.T) {
	defaults, _ := Parse(strings.NewReader(sample))
	existing, _ := Parse(strings.NewReader("OPENCLAW_DIR=/src/oc\nCLUSTER_NAME=old\nDB_MULTI_AZ=true\n"))
	values := map[string]string{
		"CLUSTER_NAME": "substrate-tui", "AWS_REGION": "us-west-2", "AWS_PROFILE": "eks-sandbox",
		"TAGS": "project=agent-substrate auto-delete=no", "MICROVM_INSTANCE_TYPE": "m8i.2xlarge",
	}
	got, err := Render(defaults, existing, values, []string{"header line"})
	if err != nil {
		t.Fatal(err)
	}
	want := `# header line

CLUSTER_NAME=substrate-tui
AWS_REGION=us-west-2
AWS_PROFILE=eks-sandbox
TAGS="project=agent-substrate auto-delete=no"

# Kept from the previous config.local.env
OPENCLAW_DIR=/src/oc
DB_MULTI_AZ=true
`
	if got != want {
		t.Errorf("Render:\n%s\nwant:\n%s", got, want)
	}
	if _, err := Render(defaults, File{}, map[string]string{"TAGS": "a=b\nc=d"}, nil); err == nil {
		t.Error("a value with a line break must be refused")
	}
}

// What Render writes reads back the same in bash, the way lib/common.sh
// sources it, and through Parse.
func TestRenderBashRoundTrip(t *testing.T) {
	values := map[string]string{
		"CLUSTER_NAME": "substrate-tui", "AWS_REGION": "us-west-2", "AWS_PROFILE": "",
		"TAGS": `project=a "quoted" $HOME back\slash ` + "`tick`", "EKS_PUBLIC_ACCESS_CIDRS": "203.0.113.0/24 198.51.100.0/24",
	}
	text, err := Render(File{}, File{}, values, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.local.env")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	back, err := ParseFile(p)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	for k, v := range values {
		if got, _ := back.Get(k); got != v {
			t.Errorf("Parse round trip %s = %q, want %q", k, got, v)
		}
	}
	for k, v := range values {
		out, err := exec.Command("bash", "-c", `. "$1"; printf %s "${!2}"`, "bash", p, k).Output()
		if err != nil {
			t.Fatalf("bash: %v", err)
		}
		if string(out) != v {
			t.Errorf("bash %s = %q, want %q", k, out, v)
		}
	}
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.local.env")
	bak := filepath.Join(dir, ".work", "installer", "config.local.env.bak")
	if err := Write(p, "A=1\n", bak); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Error("no backup is made when there was no file")
	}
	if err := Write(p, "A=2\n", bak); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bak); string(b) != "A=1\n" {
		t.Errorf("backup = %q", b)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(p); string(b) != "A=2\n" {
		t.Errorf("file = %q", b)
	}
}

func TestValidate(t *testing.T) {
	good := map[string]string{
		"CLUSTER_NAME": "substrate-tui", "AWS_REGION": "us-west-2", "AWS_PROFILE": "eks-sandbox", "KUBERNETES_VERSION": "1.37",
		"SYSTEM_INSTANCE_TYPE": "m8i.xlarge", "SYSTEM_NODES": "2", "GVISOR_INSTANCE_TYPE": "m8i.2xlarge", "GVISOR_NODES": "2",
		"MICROVM_INSTANCE_TYPE": "m8i.2xlarge", "MICROVM_NODES": "2", "TAGS": "project=agent-substrate auto-delete=no",
		"EKS_PUBLIC_ACCESS_CIDRS": "0.0.0.0/0", "SUBSTRATE_TAG": "v1.0.0",
	}
	if bad := Validate(good); len(bad) != 0 {
		t.Fatalf("good values rejected: %v", bad)
	}
	for k, v := range map[string]string{
		"CLUSTER_NAME": "Substrate_TUI", "AWS_REGION": "uswest2", "KUBERNETES_VERSION": "latest", "SYSTEM_NODES": "0",
		"GVISOR_INSTANCE_TYPE": "big", "TAGS": "no spaces=a b=", "EKS_PUBLIC_ACCESS_CIDRS": "", "SUBSTRATE_TAG": "main",
	} {
		vals := map[string]string{}
		for a, b := range good {
			vals[a] = b
		}
		vals[k] = v
		if _, ok := Validate(vals)[k]; !ok {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
}
