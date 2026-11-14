package main

import (
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

// Every plugin the installer imports registers consistently: what it
// requires exists and installs before it.
func TestPluginRegistry(t *testing.T) {
	if err := plugin.Check(); err != nil {
		t.Fatal(err)
	}
	for _, p := range plugin.All() {
		if p.Title == "" || p.Summary == "" {
			t.Errorf("plugin %s has no title or summary for the wizard", p.ID)
		}
		if p.Deploy == nil && !p.SetupAWS && p.Needs.Source == nil {
			t.Errorf("plugin %s does nothing", p.ID)
		}
	}
}
