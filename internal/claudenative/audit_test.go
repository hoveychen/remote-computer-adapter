package claudenative

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAuditRuntimeIsQuietOnAKnownLayout(t *testing.T) {
	home := t.TempDir()
	// What a real session leaves behind: the harness's own config, its session
	// records, and a cache. None of it is state this design claims to mediate.
	for _, dir := range []string{"config/projects", "config/sessions", "config/backups",
		"user-home/Library/Caches"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "config", ".claude.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found := AuditRuntime(home); len(found) != 0 {
		t.Fatalf("reported %v on a layout with no unmediated state", found)
	}
}

func TestAuditRuntimeReportsUnmediatedState(t *testing.T) {
	home := t.TempDir()
	memory := filepath.Join(home, "config", "memory")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory, "note.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	plugins := filepath.Join(home, "user-home", ".claude", "plugins")
	if err := os.MkdirAll(plugins, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugins, "p.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := AuditRuntime(home)
	if !slices.Contains(found, memory) {
		t.Errorf("missed %s in %v", memory, found)
	}
	if !slices.Contains(found, plugins) {
		t.Errorf("missed %s in %v", plugins, found)
	}
}

// An empty directory is a harness that created a location without using it.
// Reporting it would train the operator to ignore the warning.
func TestAuditRuntimeIgnoresEmptyRoots(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "config", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if found := AuditRuntime(home); len(found) != 0 {
		t.Fatalf("reported an unused directory: %v", found)
	}
}
