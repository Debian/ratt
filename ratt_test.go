package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"pault.ag/go/debian/version"
)

func writeIndex(t *testing.T, dir, name, contents string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sortedSourceNames(rebuild map[string][]version.Version) []string {
	names := make([]string, 0, len(rebuild))
	for name := range rebuild {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestTransitionAffectedSourcesSelectsSourceVersionsAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	packagesPath := writeIndex(t, dir, "Packages", `Package: foo-bin
Source: foo
Version: 1.0-1+b1
Architecture: amd64
Depends: libaffected1 (>= 1), libc6

Package: foo-tools
Source: foo
Version: 1.0-1+b1
Architecture: amd64
Depends: libaffected1, libc6

Package: bar-bin
Source: bar
Version: 2.0-1
Architecture: amd64
Depends: libunrelated1

`)
	sourcesPath := writeIndex(t, dir, "Sources", `Package: foo
Binary: foo-bin, foo-tools
Version: 1.0-1

Package: bar
Binary: bar-bin
Version: 2.0-1

`)

	rebuild, err := transitionAffectedSources([]string{packagesPath}, []string{sourcesPath}, `^libaffected1$`)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := sortedSourceNames(rebuild), []string{"foo"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("selected sources = %v, want %v", got, want)
	}
	if got, want := rebuild["foo"][0].String(), "1.0-1"; got != want {
		t.Fatalf("foo version = %q, want %q", got, want)
	}
}

func TestTransitionAffectedSourcesMapsBinaryToSourcePackage(t *testing.T) {
	dir := t.TempDir()
	packagesPath := writeIndex(t, dir, "Packages", `Package: self-src
Version: 1.0-1
Architecture: amd64
Depends: libaffected1

Package: nmu-bin
Source: nmu-src (2.0-1)
Version: 2.0-1+b1
Architecture: amd64
Depends: libaffected1

`)
	sourcesPath := writeIndex(t, dir, "Sources", `Package: self-src
Binary: self-src
Version: 1.0-1

Package: nmu-src
Binary: nmu-bin
Version: 2.0-1

`)

	rebuild, err := transitionAffectedSources([]string{packagesPath}, []string{sourcesPath}, `^libaffected1$`)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := sortedSourceNames(rebuild), []string{"nmu-src", "self-src"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("selected sources = %v, want %v", got, want)
	}
}

func TestTransitionAffectedSourcesKeepsVersionsFromMultipleSourceIndexes(t *testing.T) {
	dir := t.TempDir()
	packagesPath := writeIndex(t, dir, "Packages", `Package: foo-bin
Source: foo
Version: 1.0-1
Architecture: amd64
Depends: libaffected1

`)
	unstableSourcesPath := writeIndex(t, dir, "Sources.unstable", `Package: foo
Binary: foo-bin
Version: 1.0-1

`)
	experimentalSourcesPath := writeIndex(t, dir, "Sources.experimental", `Package: foo
Binary: foo-bin
Version: 1.1-1

`)

	rebuild, err := transitionAffectedSources(
		[]string{packagesPath},
		[]string{unstableSourcesPath, experimentalSourcesPath},
		`^libaffected1$`,
	)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := len(rebuild["foo"]), 2; got != want {
		t.Fatalf("foo versions count = %d, want %d", got, want)
	}
	if got, want := rebuild["foo"][0].String(), "1.0-1"; got != want {
		t.Fatalf("first foo version = %q, want %q", got, want)
	}
	if got, want := rebuild["foo"][1].String(), "1.1-1"; got != want {
		t.Fatalf("second foo version = %q, want %q", got, want)
	}
}

func TestTransitionAffectedSourcesOmitsSelectedSourceMissingFromSources(t *testing.T) {
	dir := t.TempDir()
	packagesPath := writeIndex(t, dir, "Packages", `Package: missing-bin
Source: missing-src
Version: 1.0-1
Architecture: amd64
Depends: libaffected1

`)
	sourcesPath := writeIndex(t, dir, "Sources", `Package: unrelated-src
Binary: unrelated-bin
Version: 1.0-1

`)

	var logs bytes.Buffer
	oldLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldLogOutput)

	rebuild, err := transitionAffectedSources([]string{packagesPath}, []string{sourcesPath}, `^libaffected1$`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rebuild) != 0 {
		t.Fatalf("selected sources = %v, want none", sortedSourceNames(rebuild))
	}
	if !strings.Contains(logs.String(), `Warning: source package "missing-src" selected by -transition_affected was not found in any Sources index`) {
		t.Fatalf("missing source warning not logged; logs:\n%s", logs.String())
	}
}

func TestTransitionAffectedSourcesIgnoresPackagesWithoutMatchingDepends(t *testing.T) {
	dir := t.TempDir()
	packagesPath := writeIndex(t, dir, "Packages", `Package: no-depends
Source: no-depends-src
Version: 1.0-1
Architecture: amd64

Package: unrelated
Source: unrelated-src
Version: 1.0-1
Architecture: amd64
Depends: libunrelated1

`)
	sourcesPath := writeIndex(t, dir, "Sources", `Package: no-depends-src
Binary: no-depends
Version: 1.0-1

Package: unrelated-src
Binary: unrelated
Version: 1.0-1

`)

	rebuild, err := transitionAffectedSources([]string{packagesPath}, []string{sourcesPath}, `^libaffected1$`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rebuild) != 0 {
		t.Fatalf("selected sources = %v, want none", sortedSourceNames(rebuild))
	}
}

func TestTransitionAffectedSourcesRejectsInvalidRegex(t *testing.T) {
	if _, err := transitionAffectedSources(nil, nil, `[`); err == nil {
		t.Fatal("transitionAffectedSources accepted an invalid regex")
	}
}
