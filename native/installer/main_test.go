package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Review F7: the installer log is 0600 in a 0700 directory, whether it is
// new or was left wider by an earlier run, and what it held is kept.
func TestCreatePrivateLogFile(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "logs", "a.log")
	if err := createPrivate(fresh); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "b.log")
	if err := os.WriteFile(old, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := createPrivate(old); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fresh, old} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v (%v), want 0600", p, st.Mode().Perm(), err)
		}
	}
	if st, _ := os.Stat(filepath.Dir(fresh)); st.Mode().Perm() != 0o700 {
		t.Errorf("log dir mode %v, want 0700", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(old); string(b) != "kept\n" {
		t.Errorf("existing log content lost: %q", b)
	}
}
