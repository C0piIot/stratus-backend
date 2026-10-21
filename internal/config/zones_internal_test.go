package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestZonesAreReadFromTheDatabase is the filter over what a zoneinfo
// directory holds besides zones.
func TestZonesAreReadFromTheDatabase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{
		"Europe/Madrid", "America/New_York", "UTC", "posix/Europe/Madrid", "right/UTC",
		"zone.tab", "tzdata.zi", "leapseconds", "Factory", "Not/AZone",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"America/New_York", "Europe/Madrid", "UTC"}
	if got := zonesIn(root); !slices.Equal(got, want) {
		t.Errorf("zonesIn = %v, want %v", got, want)
	}
	if got := zonesIn(filepath.Join(root, "nowhere")); len(got) != 0 {
		t.Errorf("a database that is not there gave %v", got)
	}
	// $ZONEINFO first, and the first database with anything in it wins.
	nowhere := filepath.Join(root, "nowhere")
	if got := zonesFrom(root, []string{nowhere}); !slices.Equal(got, want) {
		t.Errorf("zonesFrom with $ZONEINFO = %v", got)
	}
	if got := zonesFrom("", []string{nowhere, root}); !slices.Equal(got, want) {
		t.Errorf("zonesFrom past an empty one = %v", got)
	}
	if got := zonesFrom("", []string{nowhere}); got != nil {
		t.Errorf("zonesFrom with nothing = %v", got)
	}
	// A directory that cannot be read is one with no zones in it.
	locked := filepath.Join(root, "Locked")
	if err := os.MkdirAll(filepath.Join(locked, "Zone"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if got := zonesIn(root); !slices.Equal(got, want) {
		t.Errorf("zonesIn past an unreadable directory = %v", got)
	}
	// Whatever this machine has, every name in it is a zone.
	for _, z := range Zones() {
		if z == "" || z[0] < 'A' || z[0] > 'Z' {
			t.Errorf("Zones has %q", z)
		}
	}
}
