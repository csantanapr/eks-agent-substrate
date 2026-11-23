package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/eksoverlay"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/mode3"
)

// kubectlAnswers, when set, makes this test binary, run as kubectl, a
// cluster: each line is "<part of the argv>=<stdout>", and the first line
// whose part is in the argv answers it (no line: empty stdout). The answer
// kubectlFails makes that read fail instead.
const kubectlAnswers = "SUBSTRATE_EKS_TEST_KUBECTL_ANSWERS"

const kubectlFails = "!fails"

func answerKubectl(answers string) {
	argv := strings.Join(os.Args[1:], " ")
	for _, l := range strings.Split(answers, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok && strings.Contains(argv, k) {
			if v == kubectlFails {
				fmt.Fprintln(os.Stderr, "error: the server is currently unable to handle the request")
				os.Exit(1)
			}
			fmt.Print(v)
			break
		}
	}
	os.Exit(0)
}

// cluster is the kubectl answers of a cluster whose record lists record and
// whose control plane runs installed ("" none).
func cluster(record, installed string) string {
	// The nodes of the three node groups (plugin.PoolsArgs), and the
	// record as kubectl prints it (plugin.RecordArgs): nothing when it lists
	// no plugin.
	a := []string{"substrate-eks-plugins=", "get nodes -l ate.dev/pool=system gvisor microvm"}
	if record != "" {
		a[0] += `{"data":{"plugins":"` + record + `"}}`
	}
	if installed != "" {
		a = append(a, "get namespace ate-system=ate-system", "get daemonset atelet=daemonset.apps/atelet",
			"get deployment ate-api-server=ghcr.io/x/ate-api-server:"+installed)
	}
	return strings.Join(a, "\n")
}

// runMain runs main() with args against an offline account (EKS a local
// server, kubectl this test binary answering for the cluster) and returns
// its exit error and output.
func runMain(t *testing.T, answers string, args ...string) (error, string) {
	t.Helper()
	return runMainIn(t, "", nil, answers, args...)
}

// runMainIn is runMain with the child working in cwd ("" this test's own)
// and extra added to its environment, last, so it wins.
func runMainIn(t *testing.T, cwd string, extra []string, answers string, args ...string) (error, string) {
	t.Helper()
	return runMainAt(t, cwd, "", extra, answers, args...)
}

// runMainAt is runMainIn with the trees under treeRoot ("" a new empty
// directory).
func runMainAt(t *testing.T, cwd, treeRoot string, extra []string, answers string, args ...string) (error, string) {
	t.Helper()
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"cluster":{"name":"demo","version":"1.37","status":"ACTIVE","endpoint":"https://127.0.0.1:1","certificateAuthority":{"data":"Y2E="}}}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if treeRoot == "" {
		treeRoot = filepath.Join(dir, "trees")
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(bin, "kubectl")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestSubcommandHelper$")
	cmd.Env = append(os.Environ(),
		mainArgsEnv+"="+strings.Join(append(args, "--cluster", "demo", "--region", "us-west-2", "--profile", "", "--tree-root", treeRoot), "\n"),
		"AWS_PROFILE=", "AWS_CONFIG_FILE="+dir+"/none", "AWS_SHARED_CREDENTIALS_FILE="+dir+"/none",
		"AWS_ACCESS_KEY_ID=placeholder", "AWS_SECRET_ACCESS_KEY=placeholder", "AWS_SESSION_TOKEN=",
		"AWS_EC2_METADATA_DISABLED=true", "AWS_ENDPOINT_URL="+srv.URL, "XDG_CACHE_HOME="+dir,
		// Empty is unset to main's conf(): --sandbox-classes, --tag and
		// --image-repo take their defaults (gvisor,microvm, v1.0.0 and
		// upstream's images) whatever a checkout's config.local.env says.
		"SANDBOX_CLASSES=", "SUBSTRATE_TAG=", "SUBSTRATE_IMAGE_REPO=",
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), kubectlAnswers+"="+answers)
	cmd.Env, cmd.Dir = append(cmd.Env, extra...), cwd
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("still running after 60s:\n%s", out.String())
	}
	return err, out.String()
}

// The subcommands read the release off the cluster's record, both ways: with
// mode3 recorded, --deploy-demo works on the fork's release (its tree is the
// fork's); with nothing recorded, on --tag's. Each run here stops at the
// missing tree, which names the release the demo would have used.
func TestDemoRunsOnTheRecordedRelease(t *testing.T) {
	// The tree is the one the release lookup locates for this checkout's
	// deploy/eks and the Region, as the control plane step put it.
	digest, err := eksoverlay.Digest("../deploy/eks")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ record, installed, tag, repo string }{
		{mode3.ID, mode3.Tag, mode3.Tag, mode3.ImageRepo},
		{"", "v1.0.0", "v1.0.0", "ghcr.io/agent-substrate/substrate"},
		{"", "", "v1.0.0", "ghcr.io/agent-substrate/substrate"},
	} {
		rel := plugin.ChosenRelease(plugin.ParseRecord(tc.record), "v1.0.0", "ghcr.io/agent-substrate/substrate")
		tree := filepath.Base(rel.Locate("/any", digest, "us-west-2").Tree)
		err, out := runMain(t, cluster(tc.record, tc.installed), "--deploy-demo")
		if err == nil || !strings.Contains(out, ReleaseLine+tc.tag+" ("+tc.repo+")") ||
			!strings.Contains(out, "no substrate "+tc.tag+" tree with demos/counter at ") || !strings.Contains(out, "/"+tree+"; ") {
			t.Errorf("record %q: %v\n%s\nwant the tree %s", tc.record, err, out, tree)
		}
	}
}

// Through main(), the CLI refuses what the cluster cannot run: the counter
// demo on a class whose node group's nodes are missing (here gvisor, on a
// cluster with system and microvm nodes), and the AWS Nitro sandbox class
// on a cluster without Auto Mode (the fake cluster has it off), before
// anything is fetched or applied. With the nodes there the demo goes on to
// its tree (TestDemoRunsOnTheRecordedRelease).
func TestCLIRefusesClassesTheClusterCannotRun(t *testing.T) {
	answers := "substrate-eks-plugins=\nget nodes -l ate.dev/pool=system microvm"
	err, out := runMain(t, answers, "--deploy-demo", "--sandbox-classes", "gvisor")
	if err == nil || !strings.Contains(out, "the counter demo on gvisor runs on the gvisor node group, and demo has no node labeled ate.dev/pool=gvisor") {
		t.Errorf("demo on gvisor without its nodes: %v\n%s", err, out)
	}
	err, out = runMain(t, answers, "--deploy-control-plane", "--sandbox-classes", "nitro", "--database", "bundled",
		"--snap-bucket", "demo-snap", "--asset-bucket", "demo-assets", "--eks-overlay", "../deploy/eks")
	if err == nil || !strings.Contains(out, "plugin mode3-iso needs EKS Auto Mode") || strings.Contains(out, "Fetching substrate") {
		t.Errorf("Nitro class off Auto Mode: %v\n%s", err, out)
	}
}

// Both ways, through main(): db-rotation on a control plane on the bundled
// PostgreSQL (StatefulSet ate-system/postgres exists) is refused, deploy and
// verify alike, before the plugin's first step: no "Deploying plugin", so
// its AWS half never runs. With RDS (no StatefulSet) verify goes on to the
// plugin's own check; with no control plane a deploy is refused before the
// plugin's first step too. A failed read of the StatefulSet stops a deploy or a
// verify the same way, never read as RDS; a removal and the demo's check do
// not read it, so it does not stop them (review of 1ca284d, NIT-4).
func TestCLIRefusesDBRotationOnTheBundledDatabase(t *testing.T) {
	overlay, err := filepath.Abs("../deploy/eks")
	if err != nil {
		t.Fatal(err)
	}
	bundled := cluster("", "v1.0.0") + "\nget statefulset postgres=statefulset.apps/postgres"
	for _, action := range []string{"deploy", "verify"} {
		err, out := runMain(t, bundled, "--plugin-action", action, "--plugins", "db-rotation", "--eks-overlay", overlay)
		if err == nil || !strings.Contains(out, "plugin db-rotation rotates the RDS logins, so it needs the RDS database; demo's control plane uses the bundled PostgreSQL") ||
			strings.Contains(out, plugin.DeployLine) || strings.Contains(out, plugin.VerifyLine) {
			t.Errorf("%s on the bundled PostgreSQL: %v\n%s", action, err, out)
		}
	}
	_, out := runMain(t, cluster("", "v1.0.0"), "--plugin-action", "verify", "--plugins", "db-rotation", "--eks-overlay", overlay)
	if strings.Contains(out, "uses the bundled PostgreSQL") || !strings.Contains(out, plugin.VerifyLine+"db-rotation") {
		t.Errorf("verify with RDS did not reach the plugin's check:\n%s", out)
	}
	// No control plane: its AWS half is not made either (review of
	// 1ca284d, NIT-5).
	err, out = runMain(t, cluster("", ""), "--plugin-action", "deploy", "--plugins", "db-rotation", "--eks-overlay", overlay)
	if err == nil || !strings.Contains(out, "no control plane is installed on demo") || !strings.Contains(out, "run --deploy-control-plane --database rds first") ||
		strings.Contains(out, plugin.DeployLine) {
		t.Errorf("deploy with no control plane: %v\n%s", err, out)
	}

	unread := cluster("", "v1.0.0") + "\nget statefulset postgres=" + kubectlFails
	for _, action := range []string{"deploy", "verify"} {
		err, out := runMain(t, unread, "--plugin-action", action, "--plugins", "db-rotation", "--eks-overlay", overlay)
		if err == nil || !strings.Contains(out, "read whether demo runs the bundled PostgreSQL") ||
			strings.Contains(out, plugin.DeployLine) || strings.Contains(out, plugin.VerifyLine) {
			t.Errorf("%s with the database unread: %v\n%s", action, err, out)
		}
	}
	err, out = runMain(t, unread, "--plugin-action", "remove", "--eks-overlay", overlay)
	if err != nil || strings.Contains(out, "bundled PostgreSQL") {
		t.Errorf("remove with the database unread: %v\n%s", err, out)
	}
	err, out = runMain(t, unread, "--verify")
	if err == nil || strings.Contains(out, "bundled PostgreSQL") || !strings.Contains(out, "no substrate v1.0.0 tree with demos/counter") {
		t.Errorf("--verify with the database unread: %v\n%s", err, out)
	}
}

// Both ways: a deploy (--deploy-demo, --plugin-action deploy) on a release
// the control plane does not run is refused before it applies anything, with
// the fix: here mode3 was removed, so the record names no fork while the
// control plane still runs it. A check (--verify) is not a deploy and goes on.
func TestDeploysAreRefusedOnAnotherRelease(t *testing.T) {
	answers := cluster("", mode3.Tag)
	for _, args := range [][]string{{"--deploy-demo"}, {"--deploy-demo", "--verify"}, {"--plugin-action", "deploy", "--plugins", "llm-gateway"}} {
		err, out := runMain(t, answers, args...)
		if err == nil || !strings.Contains(out, "runs Substrate "+mode3.Tag+", not v1.0.0") ||
			!strings.Contains(out, "run --remove-control-plane, then install again with or without --plugins mode3 or mode3-iso") ||
			strings.Contains(out, "no substrate") || strings.Contains(out, "Deploying plugin") {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
	err, out := runMain(t, answers, "--verify")
	if err == nil || strings.Contains(out, "mixed-version") || !strings.Contains(out, "no substrate v1.0.0 tree with demos/counter") {
		t.Errorf("--verify: %v\n%s", err, out)
	}
}

// Both ways, through main(): --plugin-action remove reads what the control
// plane runs, so removing a plugin with needs (openclaw's ingress clients)
// while the fork still runs with mode3 no longer recorded gives the
// reinstall, the refusal's fix; on v1.0.0 it says a control plane deploy
// drops the needs. The plugin's Remove runs on the release that runs, not on
// --tag (review N1): openclaw's builds kubectl-ate from that release's tree,
// so the tree it finds missing names the running release.
func TestPluginRemovalOnAnotherReleaseGivesTheReinstall(t *testing.T) {
	for _, tc := range []struct {
		installed, tag, want, not string
	}{
		{mode3.Tag, "v1.0.0", "so a control plane deploy is refused: to drop them, run --remove-control-plane, then install again with or without --plugins mode3 or mode3-iso",
			"run --deploy-control-plane again"},
		{"v1.0.0", "v1.0.0", "run --deploy-control-plane again to drop them", "run --remove-control-plane"},
		{"v1.0.0", "v1.0.1", "so a control plane deploy is refused: to drop them, run --remove-control-plane, then install again with --tag v1.0.1",
			"run --deploy-control-plane again"},
	} {
		err, out := runMain(t, cluster("openclaw", tc.installed), "--plugin-action", "remove", "--plugins", "openclaw", "--tag", tc.tag)
		if err != nil || !strings.Contains(out, "Plugin removed: openclaw") || !strings.Contains(out, tc.want) || strings.Contains(out, tc.not) {
			t.Errorf("installed %s, --tag %s: %v\n%s", tc.installed, tc.tag, err, out)
		}
		if !strings.Contains(out, "WARNING: no Substrate "+tc.installed+" tree at ") {
			t.Errorf("installed %s, --tag %s: the removal did not run on the installed release:\n%s", tc.installed, tc.tag, out)
		}
	}
}
