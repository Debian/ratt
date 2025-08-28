package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"pault.ag/go/debian/version"
)

type sbuild struct {
	dist      string
	logDir    string
	dryRun    bool
	keepBuildLog  bool
	extraDebs []string
	extraExperimental bool
}

func (s *sbuild) buildCommandLine(sourcePackage string, version *version.Version) []string {
	target := fmt.Sprintf("%s_%s", sourcePackage, version)
	// TODO: discard resulting package immediately?
	cmd := []string{
		"sbuild",
		"--arch-all",
		"--dist=" + s.dist,
	}
	if s.extraExperimental {
		cmd = append(cmd,
			"--extra-repository='deb-src http://deb.debian.org/debian unstable main'",
			"--extra-repository='deb http://deb.debian.org/debian experimental main'",
			"--extra-repository='deb-src http://deb.debian.org/debian experimental main'",
			"--build-dep-resolver=aspcud",
			"--aspcud-criteria='-count(down),-count(changed,APT-Release:=/experimental/),-removed,-changed,-new'",
		)
	}
	if !s.keepBuildLog {
		cmd = append(cmd, "--nolog")
	}
	cmd = append(cmd, target)
	for _, filename := range s.extraDebs {
		cmd = append(cmd, fmt.Sprintf("--extra-package=%s", filename))
	}
	return cmd
}

func (s *sbuild) build(sourcePackage string, version *version.Version) *buildResult {
	result := &buildResult{
		src:     sourcePackage,
		version: version,
	}
	commandLine := s.buildCommandLine(sourcePackage, version)
	if s.dryRun {
		log.Printf("  commandline: %v\n", commandLine)
		return result
	}

	cmd := exec.Command(commandLine[0], commandLine[1:]...)
	target := fmt.Sprintf("%s_%s", sourcePackage, version)

	if !s.keepBuildLog {
		buildlog, err := os.Create(filepath.Join(s.logDir, target))
		if err != nil {
			result.err = err
			return result
		}
		defer buildlog.Close()
		cmd.Stdout = buildlog
		cmd.Stderr = buildlog
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	result.err = cmd.Run()
	if !s.keepBuildLog {
		result.logFile = filepath.Join(s.logDir, target)
	}
	return result
}
