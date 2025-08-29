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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"pault.ag/go/archive"
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

	skipFTBFS = flag.Bool("skip_ftbfs",
		false,
		"Filter out packages tagged as FTBFS from udd.debian.org")

	sbuildKeepBuildLog = flag.Bool("sbuild-keep-build-log",
		false,
		"Let sbuild create its .build log. Without this option, ratt passes sbuild's --nolog and saves console output in -log_dir instead")

	directRdeps = flag.Bool("direct-rdeps",
		false,
		"Limit reverse dependency analysis to packages that directly Build-Depend on the target. Equivalent to -rdeps-depth=2")

	rdepsDepth = flag.Int("rdeps-depth",
		0,
		"Set the maximum depth for reverse dependency resolution. For more details, see the --depth option in the dose-ceve(1) manpage")

	jsonOutput = flag.Bool("json",
		false,
		"Output results in JSON format (currently only works in combination with -dry_run)")

	listsPrefixRe = regexp.MustCompile(`/([^/]*_dists_.*)_InRelease$`)
)

type dryRunBuild struct {
	Package       string `json:"package"`
	Version       string `json:"version"`
	SbuildCommand string `json:"sbuild_command"`
}

type ftbfsBug struct {
	Source string `json:"source"`
}

func getFTBFSFromUDD(codename string) (map[string]struct{}, error) {
	baseURL := "https://udd.debian.org/bugs/"
	params := url.Values{
		"release":             {codename},
		"ftbfs":               {"only"},
		"notmain":             {"ign"},
		"merged":              {""},
		"fnewerval":           {"7"},
		"flastmodval":         {"7"},
		"rc":                  {"1"},
		"sortby":              {"id"},
		"caffected_packages":  {"1"},
		"sorto":               {"asc"},
		"format":              {"json"},
	}

	fullURL := baseURL + "?" + params.Encode()

	resp, err := http.Get(fullURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch FTBFS list: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("received non-OK response from UDD: %s", resp.Status)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var bugs []ftbfsBug
	if err := json.Unmarshal(body, &bugs); err != nil {
		return nil, fmt.Errorf("failed to decode FTBFS JSON: %w", err)
	}

	ftbfsSet := make(map[string]struct{}, len(bugs))
	for _, bug := range bugs {
		ftbfsSet[bug.Source] = struct{}{}
	}

	return ftbfsSet, nil
}

func fetchCodenameFromDist(dist string) (string, error) {
	url := fmt.Sprintf("http://deb.debian.org/debian/dists/%s/Release", dist)

	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch Release file: %w", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Codename: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Codename: ")), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading Release file: %w", err)
	}

	return "", fmt.Errorf("unable to find Codename field in Release file for %s", dist)
}

// Map a codename (for instance: "bookworm") to its suite ("stable", "oldstable", ...),
// using the release metadata provided by pault.ag/go/archive
func codenameToSuite(codename string) (string, error) {
	suites := []string{"stable", "oldstable"}
	for _, s := range suites {
		rel, _, err := archive.CachedRelease(s)
		if err != nil {
			return "", fmt.Errorf("fetching release for suite %q: %w", s, err)
		}
		if rel.Codename == codename {
			return rel.Suite, nil
		}
	}
	return "", fmt.Errorf("no suite found for codename %q", codename)
}

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

	if *directRdeps && *rdepsDepth != 0 && *rdepsDepth != 2 {
		log.Printf("Warning: --direct-rdeps is ignored because --rdeps-depth=%d is also set", *rdepsDepth)
	}
	if *directRdeps && *rdepsDepth == 0 {
		*rdepsDepth = 2
	}
	if *rdepsDepth > 0 {
		log.Printf("Using --depth=%d for dose-ceve(1) reverse dependency closure", *rdepsDepth)
		ceve.Args = append(ceve.Args, fmt.Sprintf("--depth=%d", *rdepsDepth))
	}

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

func getIndexPathsForDist(targetCodename, chdistInstance string) (sourcesPaths []string, packagesPaths []string) {
	var indexCodenames []string
	switch targetCodename {
	case "unstable":
		indexCodenames = []string{"sid"}
	case "experimental":
		indexCodenames = []string{"sid", "rc-buggy"}
	default:
		// treat as a concrete codename (for instance, "bookworm"). If it is part of
		// stable or oldstable, include maintenance pockets (-updates/-security).
		suite, err := codenameToSuite(targetCodename)
		if err != nil {
			log.Printf("Warning: could not resolve Suite for %q: %v (using only %q)",
				targetCodename, err, targetCodename)
			indexCodenames = []string{targetCodename}
		} else if suite == "stable" || suite == "oldstable" {
			indexCodenames = []string{
				targetCodename,
				targetCodename + "-updates",
				targetCodename + "-security",
			}
		} else {
			indexCodenames = []string{targetCodename}
		}
	}

	for _, codename := range indexCodenames {
		var srcs, pkgs []string
		if chdistInstance != "" {
			srcs, pkgs = getChdistIndexPaths(codename, chdistInstance)
		} else {
			srcs, pkgs = getAptIndexPaths(codename)
		}
		sourcesPaths = append(sourcesPaths, srcs...)
		packagesPaths = append(packagesPaths, pkgs...)
	}
	return
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

// returns the dist sbuild should use. we build experimental against unstable.
func normalizeSbuildDist(dist string) string {
	if dist == "experimental" {
		return "unstable"
	}
	return dist
}

func main() {
	flag.Parse()

	if *jsonOutput && !*dryRun {
		log.Fatal("-json can only be used together with -dry_run")
	}

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
		log.Printf("Setting -dist=%s (from .changes file)\n", *dist)
	}

	var sourcesPaths, packagesPaths []string

	if *useChdist != "" {
		sourcesPaths, packagesPaths = getIndexPathsForDist(*dist, *useChdist)
	} else {
		sourcesPaths, packagesPaths = getIndexPathsForDist(*dist, "")
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

	if *skipFTBFS {
		codename, err := fetchCodenameFromDist(*dist)
		if err != nil {
			log.Fatalf("Could not determine codename for dist %s: %v", *dist, err)
		}

		ftbfsMap, err := getFTBFSFromUDD(codename)
		if err != nil {
			log.Printf("Warning: could not fetch FTBFS list from udd.debian.org: %v", err)
		} else {
			for pkg := range rebuild {
				if _, ok := ftbfsMap[pkg]; ok {
					log.Printf("Skipping package %q - is tagged as FTBFS according to udd.debian.org", pkg)
					delete(rebuild, pkg)
				}
			}
		}
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

	sbuildDistNorm := normalizeSbuildDist(*sbuildDist)
	extraExperimental := *sbuildDist == "experimental"

	// for stable/oldstable codenames, include maintenance pockets (-updates, -security)
	extraPockets := false
	pocketsCodename := ""
	if suite, err := codenameToSuite(*sbuildDist); err == nil {
		if suite == "stable" || suite == "oldstable" {
			extraPockets = true
			pocketsCodename = *sbuildDist
		}
	} else {
		log.Printf("Warning: could not resolve Suite for %q: %v (no -updates/-security overlays)",
			*sbuildDist, err)
	}

	builder := &sbuild{
		dist:      sbuildDistNorm,
		logDir:    *logDir,
		keepBuildLog:  *sbuildKeepBuildLog,
		dryRun:    *dryRun,
		extraDebs: debs,
		extraExperimental: extraExperimental,
		extraPockets:      extraPockets,
		pocketsCodename:   pocketsCodename,
	}
	cnt := 1
	buildresults := make(map[string](*buildResult))
	var dryRunBuilds []dryRunBuild
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

		if *dryRun {
			cmd := builder.buildCommandLine(src, &newest)
			dryRunBuilds = append(dryRunBuilds, dryRunBuild{
				Package:       src,
				Version:       newest.String(),
				SbuildCommand: strings.Join(cmd, " "),
			})
		}
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

	if *dryRun && *jsonOutput {
		out, err := json.MarshalIndent(struct {
			ReverseDepCount     int           `json:"reverse_dep_count"`
			Builds              []dryRunBuild `json:"dry_run_builds"`
		}{
			Builds:          dryRunBuilds,
			ReverseDepCount: len(dryRunBuilds),
		}, "", "  ")
		if err != nil {
			log.Fatalf("Failed to marshal JSON: %v", err)
		}
		fmt.Println(string(out))
		return
	}

	if *dryRun {
		return
	}

	if *recheck {
		log.Printf("Begin to rebuild all failed packages without new changes\n")
		recheckBuilder := &sbuild{
			dist:   sbuildDistNorm,
			logDir: *logDir + "_recheck",
			keepBuildLog: *sbuildKeepBuildLog,
			dryRun: false,
			extraExperimental: extraExperimental,
			extraPockets:      extraPockets,
			pocketsCodename:   pocketsCodename,
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
