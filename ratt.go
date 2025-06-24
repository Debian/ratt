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
	"bytes"
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

func filter(m map[string][]version.Version, f func(string) bool) map[string][]version.Version {
	mf := make(map[string][]version.Version, 0)
	for k, v := range m {
		if f(k) {
			mf[k] = v
		}
	}
	return mf
}

type buildResult struct {
	src            string
	version        *version.Version
	err            error
	recheckErr     error
	logFile        string
	recheckLogFile string
}

var (
	logDir = flag.String("log_dir",
		"buildlogs",
		"Directory in which sbuild(1) logs for all reverse-build-dependencies are stored")

	dryRun = flag.Bool("dry_run",
		false,
		"Print sbuild command lines, but do not actually build the reverse-build-dependencies")

	include = flag.String("include",
		"",
		"Only build packages which match the supplied regex")

	exclude = flag.String("exclude",
		"",
		"Do not build packages which match the supplied regex")

	sbuildDist = flag.String("sbuild_dist",
		"",
		"sbuild --dist= value (e.g. \"sid\"). Defaults to the Distribution: entry from the specified .changes file")

	dist = flag.String("dist",
		"",
		"Distribution to look up reverse-build-dependencies from. Defaults to the Distribution: entry from the specified .changes file")

	recheck = flag.Bool("recheck",
		false,
		"Rebuild without new changes to check if the failures are really related")

	useChdist = flag.String("chdist",
		"",
		"Use chdist's apt-get indextargets to get sources and packages files")

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
	catFile := exec.Command("/usr/lib/apt/apt-helper",
		"cat-file",
		sourcesPath)
	var s *bufio.Reader
	if lines, err := catFile.Output(); err == nil {
		s = bufio.NewReader(bytes.NewReader(lines))
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

func fallback(sourcesPaths []string, binaries []string) (map[string][]version.Version, error) {
	bins := make(map[string]bool)
	for _, bin := range binaries {
		bins[bin] = true
	}

	rebuild := make(map[string][]version.Version)
	for _, sourcesPath := range sourcesPaths {
		if err := addReverseBuildDeps(sourcesPath, bins, rebuild); err != nil {
			return nil, err
		}
	}
	return rebuild, nil
}

func resolveAptListFile(indexFilePath string) (string, error) {
	cmd := exec.Command("/usr/lib/apt/apt-helper", "cat-file", indexFilePath)
	resolvedContent, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to read apt file %s: %w", indexFilePath, err)
	}

	tmpFile, err := os.CreateTemp("", "apt-index-resolved-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	if _, err := tmpFile.Write(resolvedContent); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write file content: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}
	return tmpFile.Name(), nil
}

func reverseBuildDeps(packagesPaths, sourcesPaths []string, binaries []string) (map[string][]version.Version, error) {
	if _, err := exec.LookPath("dose-ceve"); err != nil {
		log.Printf("dose-ceve(1) not found. Please install the dose-extra package for more accurate results. Falling back to interpreting Sources directly")
		return fallback(sourcesPaths, binaries)
	}

	archOut, err := exec.Command("dpkg-architecture", "--query=DEB_BUILD_ARCH").Output()
	if err != nil {
		log.Fatal(err)
	}
	arch := strings.TrimSpace(string(archOut))

	// TODO: Cache this output based on the .changes file. dose-ceve takes quite a while.
	ceve := exec.Command(
		"dose-ceve",
		"--deb-native-arch="+arch,
		"-T", "debsrc",
		"-r", strings.Join(binaries, ","),
		"-G", "pkg")

	for _, packagesPath := range packagesPaths {
		resolvedPath, err := resolveAptListFile(packagesPath)
		if err != nil {
			log.Printf("failed to resolve %s: %v", packagesPath, err)
			continue
		}
		defer os.Remove(resolvedPath)
		ceve.Args = append(ceve.Args, "deb://"+resolvedPath)
	}

	for _, sourcesPath := range sourcesPaths {
		resolvedPath, err := resolveAptListFile(sourcesPath)
		if err != nil {
			log.Printf("failed to resolve %s: %v", sourcesPath, err)
			continue
		}
		defer os.Remove(resolvedPath)
		ceve.Args = append(ceve.Args, "debsrc://"+resolvedPath)
	}
	ceve.Stderr = os.Stderr

	log.Printf("Figuring out reverse build dependencies using dose-ceve(1). This might take a while")
	out, err := ceve.Output()
	if err != nil {
		log.Printf("dose-ceve(1) failed (%v), falling back to interpreting Sources directly", err)
		return fallback(sourcesPaths, binaries)
	}
	var doseCeves []struct {
		Package string
		Version version.Version
	}
	r := bufio.NewReader(bytes.NewReader(out))
	if err := control.Unmarshal(&doseCeves, r); err != nil {
		return nil, err
	}
	rebuild := make(map[string][]version.Version)
	for _, doseCeve := range doseCeves {
		rebuild[doseCeve.Package] = append(rebuild[doseCeve.Package], doseCeve.Version)
	}
	return rebuild, nil
}

func fallbackIndexPaths() ([]string, []string) {
	var sourcesPaths, packagesPaths []string

	releaseMatches, err := filepath.Glob("/var/lib/apt/lists/*_InRelease")
	if err != nil {
		log.Fatal(err)
	}

	for _, releasepath := range releaseMatches {
		r, err := os.Open(releasepath)
		if err != nil {
			log.Fatal(err)
		}

		var inRelease struct{ Suite string }
		if err := control.Unmarshal(&inRelease, bufio.NewReader(r)); err != nil {
			r.Close()
			log.Fatal(err)
		}
		r.Close()

		listsPrefix := listsPrefixRe.FindStringSubmatch(releasepath)
		if len(listsPrefix) != 2 {
			log.Fatalf("release file path %q does not match regexp %q\n", releasepath, listsPrefixRe)
		}
		prefix := listsPrefix[1]

		sources, err := filepath.Glob(fmt.Sprintf("/var/lib/apt/lists/%s_*_Sources", prefix))
		if err != nil {
			log.Fatal(err)
		}
		sourcesPaths = append(sourcesPaths, sources...)

		packages, err := filepath.Glob(fmt.Sprintf("/var/lib/apt/lists/%s_*_Packages", prefix))
		if err != nil {
			log.Fatal(err)
		}
		packagesPaths = append(packagesPaths, packages...)
	}

	return sourcesPaths, packagesPaths
}

func getIndexTargets(cmdName string, args []string) ([]string, error) {
	var stderr bytes.Buffer

	cmd := exec.Command(cmdName, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("command failed: %v\nstderr:\n%s", cmd.Args, stderr.String())
	}

	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			paths = append(paths, trimmed)
		}
	}
	return paths, nil
}

func getChdistIndexPaths(dist, chdist string) ([]string, []string) {
	baseArgs := []string{"apt-get", chdist, "indextargets", "--format", "$(FILENAME)", "Codename: " + dist}

	sources, err := getIndexTargets("chdist", append(baseArgs, "ShortDesc: Sources"))
	if err != nil {
		log.Fatalf("Failed to get sources files: %v", err)
	}

	packages, err := getIndexTargets("chdist", append(baseArgs, "ShortDesc: Packages"))
	if err != nil {
		log.Fatalf("Failed to get packages files: %v", err)
	}
	return sources, packages
}

func getAptIndexPaths(dist string) ([]string, []string) {
	baseArgs := []string{"indextargets", "--format", "$(FILENAME)", "Codename: " + dist}

	sources, err := getIndexTargets("apt-get", append(baseArgs, "ShortDesc: Sources"))
	if err == nil {
		packages, err := getIndexTargets("apt-get", append(baseArgs, "ShortDesc: Packages"))
		if err != nil {
			log.Fatalf("Could not get packages files: %v", err)
		}
		return sources, packages
	}

	// Fallback: older apt-get
	return fallbackIndexPaths()
}

func main() {
	flag.Parse()

	if flag.NArg() == 0 {
		log.Fatalf("Usage: %s [options] <path-to-changes-file>...\n", os.Args[0])
	}

	var debs []string
	var binaries []string
	var changesDist string
	for i, changesPath := range flag.Args() {
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

		log.Printf(" - %d binary packages: %s\n", len(changes.Binaries), strings.Join(changes.Binaries, " "))

		for _, file := range changes.Files {
			if filepath.Ext(file.Filename) == ".deb" {
				debs = append(debs, filepath.Join(filepath.Dir(changesPath), file.Filename))
			}
		}
		binaries = append(binaries, changes.Binaries...)

		if i == 0 {
			changesDist = changes.Distribution
		} else if changesDist != changes.Distribution {
			log.Printf("%s has different distrution, but we will only consider %s\n", changes.Filename, changesDist)
		}
	}

	log.Printf("Corresponding .debs (will be injected when building):\n")
	for _, deb := range debs {
		log.Printf("    %s\n", deb)
	}

	if strings.TrimSpace(*dist) == "" {
		*dist = changesDist
		// Rewrite unstable to sid, which apt-get indextargets (below) requires.
		if *dist == "unstable" {
			*dist = "sid"
		}
		log.Printf("Setting -dist=%s (from .changes file)\n", *dist)
	}

	var sourcesPaths, packagesPaths []string

	if *useChdist != "" {
		sourcesPaths, packagesPaths = getChdistIndexPaths(*dist, *useChdist)
	} else {
		sourcesPaths, packagesPaths = getAptIndexPaths(*dist)
	}

	if len(sourcesPaths) == 0 {
		if *useChdist != "" {
			sourcesListPath := filepath.Join(os.Getenv("HOME"), ".chdist", *useChdist, "etc/apt/sources.list")
			log.Fatalf("Could not find source index files for %q using chdist %q. Are you missing this distribution in %s?", *dist, *useChdist, sourcesListPath)
		} else {
			log.Fatal("Could not find InRelease file for " + *dist + " . Are you missing " + *dist + " in your /etc/apt/sources.list?")
		}
	}

	rebuild, err := reverseBuildDeps(packagesPaths, sourcesPaths, binaries)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Found %d reverse build dependencies\n", len(rebuild))

	if *include != "" {
		filtered, err := regexp.Compile(*include)
		if err != nil {
			log.Fatal(err)
		}
		rebuild = filter(rebuild, func(v string) bool {
			return filtered.MatchString(v)
		})
		log.Printf("Based on the supplied include filter, will only build %d reverse build dependencies\n", len(rebuild))
	}
	if *exclude != "" {
		filtered, err := regexp.Compile(*exclude)
		if err != nil {
			log.Fatal(err)
		}
		rebuild = filter(rebuild, func(v string) bool {
			return !filtered.MatchString(v)
		})
		log.Printf("Based on the supplied exclude filter, will only build %d reverse build dependencies\n", len(rebuild))
	}

	// TODO: add -recursive flag to also cover dependencies which are not DIRECT dependencies. use http://godoc.org/pault.ag/go/debian/control#OrderDSCForBuild (topsort) to build dependencies in the right order (saving CPU time).

	// TODO: what’s a good integration method for doing this in more setups, e.g. on a cloud provider or something? mapreri from #debian-qa says jenkins.debian.net is suitable.

	if strings.TrimSpace(*sbuildDist) == "" {
		*sbuildDist = changesDist
		log.Printf("Setting -sbuild_dist=%s (from .changes file)\n", *sbuildDist)
	}

	if err := os.MkdirAll(*logDir, 0755); err != nil {
		log.Fatal(err)
	}

	builder := &sbuild{
		dist:      *sbuildDist,
		logDir:    *logDir,
		dryRun:    *dryRun,
		extraDebs: debs,
	}
	cnt := 1
	buildresults := make(map[string](*buildResult))
	for src, versions := range rebuild {
		sort.Sort(sort.Reverse(version.Slice(versions)))
		newest := versions[0]
		log.Printf("Building package %d of %d: %s \n", cnt, len(rebuild), src)
		cnt++
		result := builder.build(src, &newest)
		if result.err != nil {
			log.Printf("building %s failed: %v\n", src, result.err)
		}
		buildresults[src] = result
	}

	var toInclude []string
	for src, result := range buildresults {
		if result.err != nil {
			toInclude = append(toInclude, strings.ReplaceAll(src, "+", "\\+"))
		}
	}
	if len(toInclude) > 0 {
		log.Printf("%d packages failed the first pass; you can rerun ratt only for them passing the option -include '^(%s)$'\n", len(toInclude), strings.Join(toInclude, "|"))
	}

	if *dryRun {
		return
	}

	if *recheck {
		log.Printf("Begin to rebuild all failed packages without new changes\n")
		recheckBuilder := &sbuild{
			dist:   *sbuildDist,
			logDir: *logDir + "_recheck",
			dryRun: false,
		}
		if err := os.MkdirAll(recheckBuilder.logDir, 0755); err != nil {
			log.Fatal(err)
		}
		cnt := 1
		for src, result := range buildresults {
			if result.err == nil {
				continue
			}
			log.Printf("Rebuilding package %d of %d: %s \n", cnt, len(toInclude), src)
			cnt++
			recheckResult := recheckBuilder.build(src, result.version)
			result.recheckErr = recheckResult.err
			result.recheckLogFile = recheckResult.logFile
			if recheckResult.err != nil {
				log.Printf("rebuilding %s without new changes failed: %v\n", src, recheckResult.err)
			}
		}
	}

	log.Printf("Build results:\n")
	// Print all successful builds first (not as interesting), then failed ones.
	for src, result := range buildresults {
		if result.err == nil {
			log.Printf("PASSED: %s\n", src)
		}
	}

	for src, result := range buildresults {
		if result.err != nil && result.recheckErr != nil {
			log.Printf("FAILED: %s, but maybe unrelated to new changes (see %s and %s)\n",
				src, result.logFile, result.recheckLogFile)
		}
	}

	failures := false
	for src, result := range buildresults {
		if result.err != nil && result.recheckErr == nil {
			log.Printf("FAILED: %s (see %s)\n", src, result.logFile)
			failures = true
		}
	}

	if failures {
		os.Exit(1)
	}
}
