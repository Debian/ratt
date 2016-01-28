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
	"io"
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

	sbuildDist = flag.String("sbuild_dist",
		"",
		"sbuild --dist= value (e.g. \"sid\"). Defaults to the Distribution: entry from the specified .changes file")

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

type InRelease struct {
	control.Paragraph

	Origin        string
	Label         string
	Suite         string
	Codename      string
	Changelogs    string
	Date          string
	ValidUntil    string `control:"Valid-Until"`
	Architectures []string
	Components    []string
	Description   string
}

type DoseCeve struct {
	control.Paragraph

	Package string
	Version version.Version
}

func addReverseBuildDeps(sourcesPath string, binaries map[string]bool, rebuild map[string][]version.Version) error {
	log.Printf("Loading sources index %q\n", sourcesPath)
	catFile := exec.Command("/usr/lib/apt/apt-helper",
		"cat-file",
		sourcesPath)
	var s *bufio.Reader
	if lines, err := catFile.Output(); err == nil {
		s = bufio.NewReader(strings.NewReader(string(lines)))
	} else {
		// Fallback for older versions of apt-get. See
		// <20160111171230.GA17291@debian.org> for context.
		o, err := os.Open(sourcesPath)
		if err != nil {
			return err
		}
		defer o.Close()
		s = bufio.NewReader(o)
	}
	idx, err := control.ParseSourceIndex(s)
	if err != nil && err != io.EOF {
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
	if err != nil && err != io.EOF {
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
	var packagesPaths []string
	indexTargets := exec.Command("apt-get",
		"indextargets",
		"--format",
		"$(FILENAME)",
		"Codename: sid",
		"ShortDesc: Sources")
	if lines, err := indexTargets.Output(); err == nil {
		for _, line := range strings.Split(string(lines), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				sourcesPaths = append(sourcesPaths, line)
			}
		}
		binaryIndexTargets := exec.Command(
			"apt-get",
			"indextargets",
			"--format",
			"$(FILENAME)",
			"Codename: sid",
			"ShortDesc: Packages")
		lines, err = binaryIndexTargets.Output()
		if err != nil {
			log.Fatal("Could not get packages files using %+v: %v", binaryIndexTargets.Args, err)
		}
		for _, line := range strings.Split(string(lines), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				packagesPaths = append(packagesPaths, line)
			}
		}
	} else {
		// Fallback for older versions of apt-get. See
		// https://bugs.debian.org/801594 for context.
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

			inReleaase := InRelease{}
			if err := control.Unmarshal(&inReleaase, r); err != nil {
				log.Fatal(err)
			}

			if inReleaase.Suite != "unstable" {
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
			packagesMatches, err := filepath.Glob(fmt.Sprintf("/var/lib/apt/lists/%s_*_Packages", listsPrefix[1]))
			if err != nil {
				log.Fatal(err)
			}
			packagesPaths = append(packagesPaths, packagesMatches...)
		}
	}

	if len(sourcesPaths) == 0 {
		log.Fatal("Could not find InRelease file for unstable. Are you missing unstable in your /etc/apt/sources.list?")
	}

	rebuild := make(map[string][]version.Version)

	archCmd := exec.Command(
		"dpkg-architecture",
		"--query=DEB_BUILD_ARCH")
	archOut, err := archCmd.Output()
	if err != nil {
		log.Fatal(err)
	}
	arch := strings.TrimSpace(string(archOut))

	// TODO: Cache this output based on the .changes file. dose-ceve takes quite a while.
	ceve := exec.Command(
		"dose-ceve",
		"--deb-native-arch="+arch,
		"-T", "debsrc",
		"-r", strings.Join(changes.Binaries, ","),
		"-G", "pkg")
	for _, packagesPath := range packagesPaths {
		ceve.Args = append(ceve.Args, "deb://"+packagesPath)
	}
	for _, sourcesPath := range sourcesPaths {
		ceve.Args = append(ceve.Args, "debsrc://"+sourcesPath)
	}

	log.Printf("Figuring out reverse build dependencies using dose-ceve(1). This might take a while\n")
	if out, err := ceve.Output(); err == nil {
		r := bufio.NewReader(strings.NewReader(string(out)))
		doseCeves := []DoseCeve{}
		if err := control.Unmarshal(&doseCeves, r); err != nil {
			log.Fatal(err)
		}

		for _, doseCeve := range doseCeves {
			pkg := doseCeve.Package
			rebuild[pkg] = append(rebuild[pkg], doseCeve.Version)
		}
	} else {
		log.Printf("dose-ceve(1) failed (%v), falling back to interpreting Sources directly\n", err)
		for _, sourcesPath := range sourcesPaths {
			if err := addReverseBuildDeps(sourcesPath, binaries, rebuild); err != nil {
				log.Fatal(err)
			}
		}
	}

	// TODO: add -recursive flag to also cover dependencies which are not DIRECT dependencies. use http://godoc.org/pault.ag/go/debian/control#OrderDSCForBuild (topsort) to build dependencies in the right order (saving CPU time).

	// TODO: what’s a good integration method for doing this in more setups, e.g. on a cloud provider or something? mapreri from #debian-qa says jenkins.debian.net is suitable.

	if strings.TrimSpace(*sbuildDist) == "" {
		*sbuildDist = changes.Distribution
		log.Printf("Setting -sbuild_dist=%s (from .changes file)\n", *sbuildDist)
	}

	buildresults := make(map[string]bool)
	for src, versions := range rebuild {
		sort.Sort(sort.Reverse(version.Slice(versions)))
		newest := versions[0]
		target := fmt.Sprintf("%s_%s", src, newest)
		// TODO: discard resulting package immediately?
		args := []string{
			"--arch-all",
			"--dist=" + *sbuildDist,
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
