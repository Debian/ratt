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
	extraDebs []string
}

func (s *sbuild) buildCommandLine(sourcePackage string, version *version.Version) []string {
	target := fmt.Sprintf("%s_%s", sourcePackage, version)
	// TODO: discard resulting package immediately?
	cmd := []string{
		"sbuild",
		"--arch-all",
		"--dist=" + s.dist,
		"--nolog",
		target,
	}
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

	buildlog, err := os.Create(filepath.Join(s.logDir, target))
	defer buildlog.Close()
	if err != nil {
		result.err = err
		return result
	}
	cmd.Stdout = buildlog
	cmd.Stderr = buildlog
	result.err = cmd.Run()
	result.logFile = buildlog.Name()
	return result
}
