package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// zoneSources are where Go itself looks for the time zone database, in its
// order: $ZONEINFO, then the platform's directories (time/zoneinfo_unix.go).
var zoneSources = []string{"/usr/share/zoneinfo", "/usr/share/lib/zoneinfo", "/usr/lib/locale/TZ", "/etc/zoneinfo"}

// Zones lists the zones this machine knows, for the settings page to offer
// TZ from (#397): read from the same database time.LoadLocation reads rather
// than kept as a list here, and empty when there is none to read -- then the
// page takes the zone as typed.
func Zones() []string {
	return zonesFrom(os.Getenv("ZONEINFO"), zoneSources)
}

// zonesFrom is the first of the databases that has any zones in it, $ZONEINFO
// first when it is set.
func zonesFrom(zoneinfo string, sources []string) []string {
	if zoneinfo != "" {
		sources = append([]string{zoneinfo}, sources...)
	}
	for _, root := range sources {
		if zones := zonesIn(root); len(zones) > 0 {
			return zones
		}
	}
	return nil
}

// zonesIn walks one database for the names in it that are zones. The
// directory holds more than zones -- zone.tab, leapseconds, the posix/ and
// right/ copies of the whole tree -- and a name is kept only when it starts
// with a capital, has no dot in it and time.LoadLocation takes it.
func zonesIn(root string) []string {
	var out []string
	//nolint:gosec // root is $ZONEINFO or a fixed directory: the operator's, and read the way Go itself reads it
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a directory that cannot be read is one with no zones in it
		}
		name, relErr := filepath.Rel(root, path)
		if relErr != nil || name == "." {
			return nil //nolint:nilerr // the root itself
		}
		name = filepath.ToSlash(name)
		if d.IsDir() {
			if name == "posix" || name == "right" {
				return fs.SkipDir
			}
			return nil
		}
		if name[0] < 'A' || name[0] > 'Z' || strings.Contains(name, ".") || name == "Factory" {
			return nil
		}
		if _, loadErr := time.LoadLocation(name); loadErr == nil {
			out = append(out, name)
		}
		return nil
	})
	slices.Sort(out)
	return out
}
