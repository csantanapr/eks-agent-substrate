package state

import (
	"slices"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/awsx"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

func awsxCluster(name string) awsx.Cluster {
	return awsx.Cluster{Name: name, Version: "1.37", Status: "ACTIVE", AutoMode: true}
}

// Plugins for these tests, shaped like the real ones: s-kata adds the
// microvm node group (mode 3), s-nitro is the AWS Nitro sandbox class's
// (mode3-iso), s-gw runs on the system group (the LLM gateway), s-claw on
// gvisor (OpenClaw), s-vol builds on the counter demo (EFS), and s-agent
// runs on gvisor and requires s-gw (OpenClaw, which requires EFS).
func init() {
	for _, p := range []plugin.Plugin{
		{ID: "s-gw", Order: 1, Nodes: []string{"system"}},
		{ID: "s-vol", Order: 2, Nodes: []string{"system"}, NeedsDemo: true},
		{ID: "s-claw", Order: 3, Nodes: []string{"gvisor"}},
		{ID: "s-kata", Order: 4, Nodes: []string{"microvm"}, AddsNodes: true},
		{ID: "s-nitro", Order: 5, Class: ClassNitro, NeedsAutoMode: "needs EKS Auto Mode"},
		{ID: "s-agent", Order: 6, Nodes: []string{"gvisor"}, Requires: []string{"s-gw"}},
		{ID: "s-rot", Order: 7, Nodes: []string{"system"}, NeedsRDS: "rotates the RDS logins"},
		{ID: "s-iso", Order: 8, NeedsAutoMode: "needs EKS Auto Mode",
			AutoModeOptional: func(o plugin.Options) bool { return o.NitroCompute == plugin.NitroNodeGroup }},
	} {
		plugin.Register(p)
	}
}

// Every row of the rule: the node groups a new cluster gets for its
// classes, database, plugins and Auto Mode. The default (Auto Mode, RDS,
// gvisor and microvm) makes the three groups, as before the rule; with the
// bundled PostgreSQL on Auto Mode the system group comes only with a plugin
// that runs on it (or requires one that does).
func TestNewNodeGroupsEveryRow(t *testing.T) {
	const rds, bundled = DatabaseRDS, DatabaseBundled
	for _, c := range []struct {
		name     string
		auto     bool
		database string
		classes  string
		plugins  []string
		want     string
	}{
		{"default: Auto Mode, RDS, gvisor and microvm", true, rds, DefaultSandboxClasses, nil, "system,gvisor,microvm"},
		{"no database named is RDS", true, "", DefaultSandboxClasses, nil, "system,gvisor,microvm"},
		{"RDS, gvisor class", true, rds, "gvisor", nil, "system,gvisor"},
		{"RDS, microvm class", true, rds, "microvm", nil, "system,microvm"},
		{"bundled, gvisor class: no system group", true, bundled, "gvisor", nil, "gvisor"},
		{"bundled, microvm class", true, bundled, "microvm", nil, "microvm"},
		{"bundled, both classes", true, bundled, DefaultSandboxClasses, nil, "gvisor,microvm"},
		{"bundled, gvisor and a system plugin", true, bundled, "gvisor", []string{"s-gw"}, "system,gvisor"},
		{"bundled, gvisor and a plugin requiring a system one", true, bundled, "gvisor", []string{"s-agent"}, "system,gvisor"},
		{"bundled, gvisor and a gvisor plugin", true, bundled, "gvisor", []string{"s-claw"}, "gvisor"},
		{"RDS, mode 3 without the microvm class", true, rds, "gvisor", []string{"s-kata"}, "system,gvisor,microvm"},
		{"bundled, mode 3 without the microvm class", true, bundled, "gvisor", []string{"s-kata"}, "gvisor,microvm"},
		{"RDS, mode 3 with only the Nitro class", true, rds, "nitro", []string{"s-kata"}, "system,microvm"},
		{"bundled, mode 3 with only the Nitro class", true, bundled, "nitro", []string{"s-kata"}, "microvm"},
		{"RDS, Nitro class alone: no node group", true, rds, "nitro", nil, ""},
		{"bundled, Nitro class alone: no node group", true, bundled, "nitro", nil, ""},
		{"Nitro with a system plugin: the system group", true, bundled, "nitro", []string{"s-gw"}, "system"},
		{"RDS, Nitro with a system plugin", true, rds, "nitro", []string{"s-gw"}, "system"},
		{"RDS, Nitro and gvisor", true, rds, "gvisor,nitro", nil, "system,gvisor"},
		{"bundled, Nitro and gvisor", true, bundled, "gvisor,nitro", nil, "gvisor"},
		{"no Auto Mode, RDS, gvisor: system is required", false, rds, "gvisor", nil, "system,gvisor"},
		{"no Auto Mode, bundled, gvisor: system is required", false, bundled, "gvisor", nil, "system,gvisor"},
		{"no Auto Mode, default", false, rds, DefaultSandboxClasses, nil, "system,gvisor,microvm"},
	} {
		if got := strings.Join(NewNodeGroups(c.auto, c.database, c.classes, plugin.NitroAutoMode, c.plugins), ","); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		// Without the nitro class, NITRO_COMPUTE changes nothing.
		if !HasClass(c.classes, ClassNitro) {
			if got := strings.Join(NewNodeGroups(c.auto, c.database, c.classes, plugin.NitroNodeGroup, c.plugins), ","); got != c.want {
				t.Errorf("%s, node-group: %q, want %q", c.name, got, c.want)
			}
		}
	}
	// NITRO_COMPUTE=node-group: the nitro class makes the nitro group, a
	// sandbox group like the others (the system group with RDS on Auto Mode,
	// and always without Auto Mode).
	for _, c := range []struct {
		name          string
		auto          bool
		database      string
		classes, want string
	}{
		{"Auto Mode, bundled, Nitro alone", true, bundled, "nitro", "nitro"},
		{"Auto Mode, RDS, Nitro alone", true, rds, "nitro", "system,nitro"},
		{"no Auto Mode, bundled, Nitro alone", false, bundled, "nitro", "system,nitro"},
		{"Auto Mode, RDS, all three", true, rds, "gvisor,microvm,nitro", "system,gvisor,microvm,nitro"},
		{"Auto Mode, bundled, gvisor and Nitro", true, bundled, "gvisor,nitro", "gvisor,nitro"},
	} {
		if got := strings.Join(NewNodeGroups(c.auto, c.database, c.classes, plugin.NitroNodeGroup, nil), ","); got != c.want {
			t.Errorf("node-group, %s: %q, want %q", c.name, got, c.want)
		}
	}
	// Setup.NodeGroups is the same for a new cluster, with its database,
	// and a connected cluster's own groups, in order, for one it connects.
	s := &Setup{NewClusterConfig: ClusterConfig{AutoMode: "on"}, SandboxClasses: "nitro"}
	s.NewCluster("c")
	if g := s.NodeGroups(); len(g) != 0 {
		t.Errorf("new, Nitro alone: %v", g)
	}
	s.SandboxClasses = "gvisor"
	if g := s.NodeGroups(); !slices.Equal(g, []string{"system", "gvisor"}) {
		t.Errorf("new, RDS, gvisor: %v", g)
	}
	s.Database = DatabaseBundled
	if g := s.NodeGroups(); !slices.Equal(g, []string{"gvisor"}) {
		t.Errorf("new, bundled, gvisor: %v", g)
	}
	s.SelectCluster(awsxCluster("c"), []string{"microvm", "isolated", "system"}, nil, false)
	if g := s.NodeGroups(); !slices.Equal(g, []string{"system", "microvm"}) {
		t.Errorf("connected: %v", g)
	}
	// A connected cluster's nitro group comes from its nodes' labels.
	s.SelectCluster(awsxCluster("c"), []string{"nitro", "system"}, nil, false)
	if g := s.NodeGroups(); !slices.Equal(g, []string{"system", "nitro"}) {
		t.Errorf("connected with nitro nodes: %v", g)
	}
	// A new cluster with NITRO_COMPUTE=node-group makes it.
	s.NewClusterConfig.NitroCompute, s.SandboxClasses, s.Database = plugin.NitroNodeGroup, "nitro", DatabaseBundled
	s.NewCluster("c")
	if g := s.NodeGroups(); !slices.Equal(g, []string{"nitro"}) {
		t.Errorf("new, node-group, Nitro alone: %v", g)
	}
}

// The refusals: the Nitro class without Auto Mode, a class or plugin whose
// nodes a connected cluster lacks, a plugin pinned to a sandbox group whose
// class is off (with the fix), and one built on the counter demo with no
// class to run it on; each with what makes it available. A plugin on the
// system group is never refused on a new cluster: it makes the group.
func TestClassAndPluginRefusals(t *testing.T) {
	am, ng := plugin.NitroAutoMode, plugin.NitroNodeGroup
	if ClassProblem(ClassNitro, false, true, "c", nil, am) == "" || ClassProblem(ClassNitro, true, true, "c", nil, am) != "" {
		t.Error("nitro and Auto Mode")
	}
	if why := ClassProblem(ClassGVisor, true, false, "c", []string{"system", "microvm"}, am); !strings.Contains(why, "c has no gvisor node group") {
		t.Errorf("connected without gvisor: %q", why)
	}
	if ClassProblem(ClassGVisor, true, false, "c", []string{"gvisor"}, am) != "" || ClassProblem(ClassGVisor, true, true, "c", nil, am) != "" {
		t.Error("gvisor refused with its nodes, or on a new cluster")
	}
	// NITRO_COMPUTE=node-group: no Auto Mode needed on a new cluster (it
	// makes the nitro group); a connected cluster needs a node labeled
	// ate.dev/pool=nitro, with or without Auto Mode. auto-mode still needs
	// Auto Mode, and says so even where the nitro group is there.
	for _, c := range []struct {
		auto, newCluster bool
		pools            []string
		compute, want    string
	}{
		{false, true, nil, ng, ""},
		{true, true, nil, ng, ""},
		{false, false, []string{"system", "nitro"}, ng, ""},
		{true, false, []string{"nitro"}, ng, ""},
		{false, false, []string{"system"}, ng, "c has no nitro node group (no node labeled ate.dev/pool=nitro)"},
		{true, false, []string{"isolated"}, ng, "c has no nitro node group"},
		{false, true, nil, am, "needs EKS Auto Mode"},
		{false, true, nil, "", "needs EKS Auto Mode"},
		{false, false, []string{"nitro"}, am, "c has a nitro node group, which NITRO_COMPUTE=node-group uses"},
		{true, false, nil, am, ""},
	} {
		if why := ClassProblem(ClassNitro, c.auto, c.newCluster, "c", c.pools, c.compute); (c.want == "") != (why == "") || !strings.Contains(why, c.want) {
			t.Errorf("%+v: %q", c, why)
		}
	}

	get := func(id string) plugin.Plugin { p, _ := plugin.Get(id); return p }
	// The minimal profile, with either database: a plugin on the system
	// group makes it, so only a missing sandbox class or demo class refuses.
	for _, db := range []string{DatabaseRDS, DatabaseBundled} {
		s := &Setup{NewClusterConfig: ClusterConfig{AutoMode: "on"}, SandboxClasses: "nitro", Database: db}
		s.NewCluster("c")
		for id, want := range map[string]string{
			"s-gw":    "",
			"s-claw":  "runs on the gvisor node group: turn on its sandbox class",
			"s-agent": "runs on the gvisor node group: turn on its sandbox class",
			"s-vol":   "builds on the counter demo",
			"s-kata":  "",
		} {
			if why := s.PluginProblem(get(id), nil); !strings.Contains(why, want) || (want == "") != (why == "") {
				t.Errorf("minimal profile, %s, %s: %q, want %q", db, id, why, want)
			}
		}
	}
	s := &Setup{NewClusterConfig: ClusterConfig{AutoMode: "on"}, SandboxClasses: "gvisor", Database: DatabaseBundled}
	s.NewCluster("c")
	for _, id := range []string{"s-gw", "s-vol", "s-claw", "s-agent"} {
		if why := s.PluginProblem(get(id), nil); why != "" {
			t.Errorf("bundled, gvisor class, %s: %q", id, why)
		}
	}
	s.Database = DatabaseRDS
	for _, id := range []string{"s-gw", "s-vol", "s-claw"} {
		if why := s.PluginProblem(get(id), nil); why != "" {
			t.Errorf("RDS, gvisor class, %s: %q", id, why)
		}
	}
	s.NewClusterConfig.AutoMode = ""
	s.NewCluster("c")
	if why := s.PluginProblem(get("s-nitro"), nil); !strings.Contains(why, "Auto Mode off") {
		t.Errorf("Nitro's plugin off Auto Mode: %q", why)
	}
	s.SelectCluster(awsxCluster("c"), []string{"system"}, nil, false)
	if why := s.PluginProblem(get("s-claw"), nil); why != "runs on the gvisor node group, which c does not have" {
		t.Errorf("connected without gvisor: %q", why)
	}
	// A plugin that needs RDS (db-rotation) is refused with the bundled
	// PostgreSQL, new cluster or connected, and offered with RDS.
	for _, connected := range []bool{false, true} {
		r := &Setup{NewClusterConfig: ClusterConfig{AutoMode: "on"}, SandboxClasses: "gvisor", Database: DatabaseBundled}
		r.NewCluster("c")
		if connected {
			r.SelectCluster(awsxCluster("c"), []string{"system", "gvisor"}, nil, false)
		}
		if why := r.PluginProblem(get("s-rot"), nil); why != "rotates the RDS logins; the run uses the bundled PostgreSQL (Settings: Database)" {
			t.Errorf("connected=%v, bundled: %q", connected, why)
		}
		r.Database = DatabaseRDS
		if why := r.PluginProblem(get("s-rot"), nil); why != "" {
			t.Errorf("connected=%v, RDS: %q", connected, why)
		}
	}
	// A connected cluster's nodes decide: without a system node, a plugin
	// on the system group is refused, since nothing is created there.
	s.SelectCluster(awsxCluster("c"), []string{"gvisor"}, nil, false)
	if why := s.PluginProblem(get("s-gw"), nil); why != "runs on the system node group, which c does not have" {
		t.Errorf("connected without system: %q", why)
	}
}

// What ate-setup gets, from its own two classes: gvisor always (one
// SandboxConfig every gVisor-class pool resolves, the Nitro class's
// counter-iso and mode 3's among them), microvm with the microvm class.
// The counter demo runs on gvisor and microvm; the Nitro class brings its
// plugin, and the install then has a Set up Nitro sandboxes step.
func TestClassesReachAteSetupTheDemoAndThePlugins(t *testing.T) {
	for classes, want := range map[string]string{
		"gvisor,microvm": "gvisor,microvm", "gvisor": "gvisor", "microvm": "gvisor,microvm",
		"nitro": "gvisor", "gvisor,nitro": "gvisor", "microvm,nitro": "gvisor,microvm",
	} {
		if got := AteSetupClasses(classes); got != want {
			t.Errorf("AteSetupClasses(%s) = %s, want %s", classes, got, want)
		}
	}
	if got := DemoClasses("gvisor,microvm,nitro"); !slices.Equal(got, []string{"gvisor", "microvm"}) || len(DemoClasses("nitro")) != 0 {
		t.Errorf("DemoClasses = %v", got)
	}
	s := &Setup{SandboxClasses: "gvisor,nitro", Plugins: []string{"s-gw"}, Database: DatabaseBundled}
	if got := s.InstallPlugins(); !slices.Equal(got, []string{"s-gw", "s-nitro"}) {
		t.Errorf("InstallPlugins = %v", got)
	}
	o := InstallOrder(s)
	if i := slices.Index(o, NitroSandbox); i < 0 || o[i-1] != ControlPlane {
		t.Errorf("order %v", o)
	}
	s.SandboxClasses = "gvisor"
	if slices.Contains(InstallOrder(s), NitroSandbox) || len(s.InstallPlugins()) != 1 {
		t.Errorf("without the class: order %v, plugins %v", InstallOrder(s), s.InstallPlugins())
	}
}

// PluginProblem's Auto Mode refusal follows the plugin's AutoModeOptional:
// with NITRO_COMPUTE=node-group a plugin that may run off Auto Mode is
// offered there; with auto-mode it is refused as before.
func TestPluginProblemHonoursAutoModeOptional(t *testing.T) {
	get := func(id string) plugin.Plugin { p, _ := plugin.Get(id); return p }
	for _, c := range []struct {
		compute string
		want    string
	}{{plugin.NitroAutoMode, "needs EKS Auto Mode; c has Auto Mode off"}, {"", "needs EKS Auto Mode; c has Auto Mode off"}, {plugin.NitroNodeGroup, ""}} {
		s := &Setup{NewClusterConfig: ClusterConfig{AutoMode: "", NitroCompute: c.compute}, SandboxClasses: "gvisor"}
		s.NewCluster("c")
		if got := s.PluginProblem(get("s-iso"), nil); got != c.want {
			t.Errorf("NITRO_COMPUTE=%q: %q, want %q", c.compute, got, c.want)
		}
		if got := s.PluginProblem(get("s-nitro"), nil); got != "needs EKS Auto Mode; c has Auto Mode off" {
			t.Errorf("NITRO_COMPUTE=%q: a plugin without AutoModeOptional: %q", c.compute, got)
		}
	}
}
