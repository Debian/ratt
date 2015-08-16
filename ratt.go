// ratt operates on a Debian .changes file of a just-built package, identifies
// all reverse-build-dependencies and rebuilds them with the .debs from the
// .changes file.
//
// The intended use-case is, for example, to package a new snapshot of a Go
// library and verify that the new version does not break any other Go
// libraries/binaries.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"pault.ag/go/debian/control"
	"pault.ag/go/debian/version"
)

var (
	logDir = flag.String("log_dir",
		"buildlogs",
		"Directory in which sbuild(1) logs for all reverse-build-dependencies are stored")

	dryRun = flag.Bool("dry_run",
		false,
		"Print sbuild command lines, but do not actually build the reverse-build-dependencies")

	listsPrefixRe = regexp.MustCompile(`/([^/]*_dists_.*)_InRelease$`)
)

func dependsOn(src control.SourceIndex, binaries map[string]bool) bool {
	buildDepends := src.GetBuildDepends()
	for _, possibility := range buildDepends.GetAllPossibilities() {
		if binaries[possibility.Name] {
			return true
		}
	}
	return false
}

func addReverseBuildDeps(sourcesPath string, binaries map[string]bool, rebuild map[string][]version.Version) error {
	log.Printf("Loading sources index %q\n", sourcesPath)
	s, err := os.Open(sourcesPath)
	if err != nil {
		return err
	}
	defer s.Close()
	idx, err := control.ParseSourceIndex(bufio.NewReader(s))
	if err != nil {
		return err
	}

	for _, src := range idx {
		if dependsOn(src, binaries) {
			rebuild[src.Package] = append(rebuild[src.Package], src.Version)
		}
	}

	return nil
}

func main() {
	flag.Parse()

	if flag.NArg() != 1 {
		log.Fatalf("Usage: %s [options] <path-to-changes-file>\n", os.Args[0])
	}

	changesPath := flag.Arg(0)
	log.Printf("Loading changes file %q\n", changesPath)

	c, err := os.Open(changesPath)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	changes, err := control.ParseChanges(bufio.NewReader(c), changesPath)
	if err != nil {
		log.Fatal(err)
	}

	binaries := make(map[string]bool)
	var debs []string
	for _, file := range changes.Files {
		if filepath.Ext(file.Filename) == ".deb" {
			debs = append(debs, file.Filename)
		}
	}
	for _, binary := range changes.Binaries {
		binaries[binary] = true
	}

	log.Printf(" - %d binary packages: %s\n", len(changes.Binaries), strings.Join(changes.Binaries, " "))
	log.Printf(" - corresponding .debs (will be injected when building):\n")
	for _, deb := range debs {
		log.Printf("    %s\n", deb)
	}

	var sourcesPaths []string
	releaseMatches, err := filepath.Glob("/var/lib/apt/lists/*_InRelease")
	if err != nil {
		log.Fatal(err)
	}
	for _, releasepath := range releaseMatches {
		r, err := os.Open(releasepath)
		if err != nil {
			log.Fatal(err)
		}
		defer r.Close()
		release, err := control.ParseParagraph(bufio.NewReader(r))
		if err != nil {
			log.Fatal(err)
		}
		if release.Values["Suite"] != "unstable" {
			continue
		}

		listsPrefix := listsPrefixRe.FindStringSubmatch(releasepath)
		if len(listsPrefix) != 2 {
			log.Fatalf("release file path %q does not match regexp %q\n", releasepath, listsPrefixRe)
		}
		sourceMatches, err := filepath.Glob(fmt.Sprintf("/var/lib/apt/lists/%s_*_Sources", listsPrefix[1]))
		if err != nil {
			log.Fatal(err)
		}
		sourcesPaths = append(sourcesPaths, sourceMatches...)
	}

	if len(sourcesPaths) == 0 {
		log.Fatal("Could not find InRelease file for unstable. Are you missing unstable in your /etc/apt/sources.list?")
	}

	rebuild := make(map[string][]version.Version)

	for _, sourcesPath := range sourcesPaths {
		if err := addReverseBuildDeps(sourcesPath, binaries, rebuild); err != nil {
			log.Fatal(err)
		}
	}

	// TODO: add -recursive flag to also cover dependencies which are not DIRECT dependencies. use http://godoc.org/pault.ag/go/debian/control#OrderDSCForBuild (topsort) to build dependencies in the right order (saving CPU time).

	// TODO: what’s a good integration method for doing this in more setups, e.g. on a cloud provider or something? mapreri from #debian-qa says jenkins.debian.net is suitable.

	buildresults := make(map[string]bool)
	for src, versions := range rebuild {
		sort.Sort(sort.Reverse(version.Slice(versions)))
		newest := versions[0]
		target := fmt.Sprintf("%s_%s", src, newest)
		// TODO: discard resulting package immediately?
		args := []string{
			"--arch-all",
			"--dist=sid",
			"--nolog",
			target,
		}
		for _, filename := range debs {
			args = append(args, fmt.Sprintf("--extra-package=%s", filename))
		}
		cmd := exec.Command("sbuild", args...)
		if err := os.MkdirAll(*logDir, 0755); err != nil {
			log.Fatal(err)
		}
		log.Printf("Building %s (commandline: %v)\n", target, cmd.Args)
		if *dryRun {
			continue
		}
		buildlog, err := os.Create(filepath.Join(*logDir, target))
		if err != nil {
			log.Fatal(err)
		}
		defer buildlog.Close()
		cmd.Stdout = buildlog
		cmd.Stderr = buildlog
		if err := cmd.Run(); err != nil {
			log.Printf("building %s failed: %v\n", target, err)
			buildresults[target] = false
		} else {
			buildresults[target] = true
		}
	}

	log.Printf("Build results:\n")
	// Print all successful builds first (not as interesting), then failed ones.
	for target, result := range buildresults {
		if !result {
			continue
		}
		log.Printf("PASSED: %s\n", target)
	}

	for target, result := range buildresults {
		if result {
			continue
		}
		log.Printf("FAILED: %s (see %s)\n", target, filepath.Join(*logDir, target))
	}
}
