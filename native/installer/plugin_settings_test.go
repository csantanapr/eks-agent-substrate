package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

// settingsProbe, set in TestSubcommandHelper's environment, registers the
// settings-probe plugin in that child only, so no other test sees it.
const settingsProbe = "SUBSTRATE_EKS_TEST_SETTINGS_PROBE"

// registerSettingsProbe registers a plugin whose check prints each plugin
// setting as its code reads it, from the environment.
func registerSettingsProbe() {
	if _, ok := os.LookupEnv(settingsProbe); !ok {
		return
	}
	plugin.Register(plugin.Plugin{ID: "settings-probe", Order: 1000,
		Verify: func(_ context.Context, w io.Writer, _ plugin.Options) error {
			for _, k := range state.PluginKeys {
				fmt.Fprintf(w, "probe %s=%q\n", k, os.Getenv(k))
			}
			return nil
		}})
}

// Both ways, through main() (review N2): --plugin-action reads the plugin
// settings from config.env and config.local.env itself, since the wizard
// passes them to no step. With WHATSAPP_PEER only in config.local.env (and
// EFS_ADDON_VERSION only in config.env), the plugin code gets both values;
// with WHATSAPP_PEER also exported, the exported value wins over the file.
func TestPluginActionReadsPluginSettingsFromTheConfigFiles(t *testing.T) {
	// No plugin setting from this test's own environment reaches the child.
	for _, k := range state.PluginKeys {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	overlay, err := filepath.Abs("../deploy/eks")
	if err != nil {
		t.Fatal(err)
	}
	// A checkout: config.env and config.local.env beside native/tools/setup-aws.
	checkout := t.TempDir()
	for name, body := range map[string]string{
		"native/tools/setup-aws/go.mod": "module example.invalid/setup-aws\n",
		"config.env":                    "WHATSAPP_PEER=\nEFS_ADDON_VERSION=v9.9.9-eksbuild.1\n",
		"config.local.env":              "WHATSAPP_PEER=+15555550100\n",
	} {
		p := filepath.Join(checkout, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name     string
		exported []string
		want     string
	}{
		{"only in config.local.env", nil, `probe WHATSAPP_PEER="+15555550100"`},
		{"exported too", []string{"WHATSAPP_PEER=+15555550199"}, `probe WHATSAPP_PEER="+15555550199"`},
	} {
		err, out := runMainIn(t, checkout, append([]string{settingsProbe + "=1"}, tc.exported...), cluster("", "v1.0.0"),
			"--plugin-action", "verify", "--plugins", "settings-probe", "--eks-overlay", overlay)
		if err != nil || !strings.Contains(out, tc.want) || !strings.Contains(out, `probe EFS_ADDON_VERSION="v9.9.9-eksbuild.1"`) ||
			!strings.Contains(out, "Plugin ready: settings-probe") {
			t.Errorf("%s: %v, want %s and config.env's EFS_ADDON_VERSION:\n%s", tc.name, err, tc.want, out)
		}
	}
}
