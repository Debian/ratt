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
	"context"
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
	"sync"
	"time"

	"github.com/Debian/ratt/internal/pb"
	"google.golang.org/grpc"

	"golang.org/x/sync/errgroup"

	"pault.ag/go/debian/control"
	"pault.ag/go/debian/version"
)

type buildResult struct {
	err     error
	logFile string
}

type category string

const (
	alreadyBroken category = "Already broken"
	failing       category = "Failing"
	passing       category = "Passing"
)

type categorizedResult struct {
	category category
	result   string
}

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

	dist = flag.String("dist",
		"",
		"Distribution to look up reverse-build-dependencies from. Defaults to the Distribution: entry from the specified .changes file")

	recheck = flag.Bool("recheck",
		false,
		"Rebuild without new changes to check if the failures are really related")

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
		ceve.Args = append(ceve.Args, "deb://"+packagesPath)
	}
	for _, sourcesPath := range sourcesPaths {
		ceve.Args = append(ceve.Args, "debsrc://"+sourcesPath)
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

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
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
				debs = append(debs, file.Filename)
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

	var sourcesPaths []string
	var packagesPaths []string
	indexTargets := exec.Command("apt-get",
		"indextargets",
		"--format",
		"$(FILENAME)",
		"Codename: "+*dist,
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
			"Codename: "+*dist,
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
			var inRelease struct {
				Suite string
			}
			if err := control.Unmarshal(&inRelease, bufio.NewReader(r)); err != nil {
				log.Fatal(err)
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
		log.Fatal("Could not find InRelease file for " + *dist + " . Are you missing " + *dist + " in your /etc/apt/sources.list?")
	}

	rebuild, err := reverseBuildDeps(packagesPaths, sourcesPaths, binaries)
	if err != nil {
		log.Fatal(err)
	}

	// TODO: add -recursive flag to also cover dependencies which are not DIRECT dependencies. use http://godoc.org/pault.ag/go/debian/control#OrderDSCForBuild (topsort) to build dependencies in the right order (saving CPU time).

	if strings.TrimSpace(*sbuildDist) == "" {
		*sbuildDist = changesDist
		log.Printf("Setting -sbuild_dist=%s (from .changes file)\n", *sbuildDist)
	}

	if err := os.MkdirAll(*logDir, 0755); err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	// TODO: flag. iterate through the specified servers, falling back.
	conn, err := grpc.DialContext(context.Background(), "localhost:12500", grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	configuration := pb.NewConfigurationClient(conn)

	config, err := configuration.Get(ctx, &pb.GetRequest{})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("connected to localhost:12500, builder config: %+v", config)

	var (
		concurrent   = make(chan struct{}, config.GetConcurrentBuilds())
		eg           errgroup.Group
		mu           sync.Mutex
		buildresults = make(map[string]categorizedResult)
		retry        = time.Tick(1 * time.Second)
		birdseye     = birdseye{
			out:    os.Stdout,
			states: make([]birdseyeState, config.GetConcurrentBuilds()),
		}
	)
	cnt := 0
	for src, versions := range rebuild {
		cnt++
		cnt, src, versions := cnt, src, versions // copy
		cnt--                                    // make the count zero indexed
		eg.Go(func() error {
			birdseye.status(cnt, stateInit)
			defer birdseye.status(cnt, stateDone)
			sort.Sort(sort.Reverse(version.Slice(versions)))
			newest := versions[0]
			target := fmt.Sprintf("%s_%s", src, newest)
			job := struct {
				build         *sbuild
				recheck       bool
				result        *buildResult
				recheckResult *buildResult
			}{
				build: &sbuild{
					dist:      *sbuildDist,
					logDir:    *logDir,
					dryRun:    *dryRun,
					extraDebs: debs,
					target:    target,
				},
			}
			for {
				birdseye.status(cnt, stateSleep)
				concurrent <- struct{}{} // acquire semaphore
				birdseye.status(cnt, stateRun)
				result, err := job.build.build(ctx, pb.NewSemaphoreClient(conn))
				birdseye.status(cnt, stateError)
				if err != nil {
					if isTemporary(err) {
						<-retry      // rate-limit retries
						<-concurrent // release semaphore
						continue
					}
					return err // build could not be orchestrated
				}
				if !job.recheck {
					job.result = result
				} else {
					job.recheckResult = result
				}
				// build has succeeded or failed
				if result.err != nil {
					if *recheck && !job.recheck {
						job.recheck = true
						job.build.extraDebs = nil
						job.build.logDir += "_recheck"
						<-concurrent // release semaphore
						continue
					}
				}
				mu.Lock()
				if job.recheck && job.recheckResult.err != nil {
					buildresults[src] = categorizedResult{alreadyBroken, fmt.Sprintf("FAILED: %s, but maybe unrelated to new changes (see %s and %s)",
						src, job.result.logFile, job.recheckResult.logFile)}
				} else if job.result.err != nil {
					buildresults[src] = categorizedResult{failing, fmt.Sprintf("FAILED: %s (see %s)", src, job.result.logFile)}
				} else {
					buildresults[src] = categorizedResult{passing, fmt.Sprintf("PASSED: %s", src)}
				}
				mu.Unlock()
				<-concurrent // release semaphore
				break
			}
			return nil
		})
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(1 * time.Second):
				birdseye.print()
			}
		}
	}()
	if err := eg.Wait(); err != nil {
		log.Fatal(err)
	}
	close(done)

	birdseye.flush()

	log.Printf("Build results:\n")
	counts := make(map[category]int)
	for _, category := range []category{passing, alreadyBroken, failing} {
		for _, result := range buildresults {
			if result.category != category {
				continue
			}

			log.Println(result.result)
			counts[category]++
		}
	}
	var parts []string
	for _, category := range []category{passing, alreadyBroken, failing} {
		if cnt, ok := counts[category]; ok {
			parts = append(parts, fmt.Sprintf("%d %s", cnt, category))
		}
	}
	log.Println(strings.Join(parts, ", "))
}
