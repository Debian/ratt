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

func (s *sbuild) build(sourcePackage string, version *version.Version) *buildResult {
	result := &buildResult{
		src:     sourcePackage,
		version: version,
	}
	target := fmt.Sprintf("%s_%s", sourcePackage, version)
	// TODO: discard resulting package immediately?
	args := []string{
		"--arch-all",
		"--dist=" + s.dist,
		"--nolog",
		target,
	}
	for _, filename := range s.extraDebs {
		args = append(args, fmt.Sprintf("--extra-package=%s", filename))
	}
	cmd := exec.Command("sbuild", args...)
	if s.dryRun {
		log.Printf("  commandline: %v\n", cmd.Args)
		return result
	}

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
