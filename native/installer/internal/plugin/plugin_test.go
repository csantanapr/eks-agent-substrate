package plugin

import (
	"slices"
	"strings"
	"testing"
)

func withRegistry(t *testing.T, ps ...Plugin) {
	t.Helper()
	saved := registry
	registry = map[string]Plugin{}
	t.Cleanup(func() { registry = saved })
	for _, p := range ps {
		Register(p)
	}
}

func ids(ps []Plugin) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func TestResolveAddsRequirementsInOrder(t *testing.T) {
	withRegistry(t,
		Plugin{ID: "openclaw", Order: 50, Requires: []string{"efs", "llm-gateway"}},
		Plugin{ID: "efs", Order: 20},
		Plugin{ID: "llm-gateway", Order: 10},
		Plugin{ID: "s3files", Order: 30},
	)
	got, err := Resolve([]string{"openclaw"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"llm-gateway", "efs", "openclaw"}; !slices.Equal(ids(got), want) {
		t.Errorf("Resolve = %v, want %v", ids(got), want)
	}
	if _, err := Resolve([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("unknown plugin: %v", err)
	}
}

func TestResolveRefusesACycle(t *testing.T) {
	withRegistry(t, Plugin{ID: "a", Requires: []string{"b"}}, Plugin{ID: "b", Requires: []string{"a"}})
	if _, err := Resolve([]string{"a"}); err == nil || !strings.Contains(err.Error(), "a -> b -> a") {
		t.Errorf("cycle: %v", err)
	}
}

func TestMergeUnitesNeedsAndRefusesTwoSources(t *testing.T) {
	a := Plugin{ID: "a", Needs: Needs{Preview: []string{"ExternalVolumes"}, IngressIDs: []string{"spiffe://x/b", "spiffe://x/a"}}}
	b := Plugin{ID: "b", Needs: Needs{Preview: []string{"ExternalVolumes", "NetworkWorkers"}, CredentialProviders: []string{`{"name":"aws"}`},
		Source: &Source{Tag: "v1.0.0-fork"}}}
	n, err := Merge([]Plugin{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(n.Preview, []string{"ExternalVolumes", "NetworkWorkers"}) || !slices.Equal(n.IngressIDs, []string{"spiffe://x/a", "spiffe://x/b"}) ||
		len(n.CredentialProviders) != 1 || n.Source == nil || n.Source.Tag != "v1.0.0-fork" {
		t.Errorf("Merge = %+v", n)
	}
	c := Plugin{ID: "c", Needs: Needs{Source: &Source{Tag: "other"}}}
	if _, err := Merge([]Plugin{b, c}); err == nil {
		t.Error("two Sources merged")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	if got := ParseRecord(" efs, ,llm-gateway,efs "); !slices.Equal(got, []string{"efs", "llm-gateway"}) {
		t.Errorf("ParseRecord = %v", got)
	}
	if got := FormatRecord([]string{"s3files", "efs", "efs"}); got != "efs,s3files" {
		t.Errorf("FormatRecord = %q", got)
	}
}

func TestRegisterRefusesDuplicatesAndBadIDs(t *testing.T) {
	for _, bad := range []string{"", "a b", "a,b"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) did not panic", bad)
				}
			}()
			withRegistry(t, Plugin{ID: bad})
		}()
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("duplicate did not panic")
			}
		}()
		withRegistry(t, Plugin{ID: "x"}, Plugin{ID: "x"})
	}()
}
