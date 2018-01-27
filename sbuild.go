package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Debian/ratt/internal/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type sbuild struct {
	dist      string
	logDir    string
	dryRun    bool
	extraDebs []string
	target    string
}

type semaphoreAcquireError struct {
	err error
}

func (e *semaphoreAcquireError) Error() string {
	return fmt.Sprintf("acquiring build semaphore: %v", e.err)
}

func isTemporary(err error) bool {
	if _, ok := err.(*semaphoreAcquireError); ok {
		return true
	}
	if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
		return true
	}
	return false
}

func writeFile(stream pb.Build_WriteFileClient, buildId, fn string) error {
	const chunkSize = 3 * 1024 * 1024 // under the 4 MiB gRPC message size limit

	f, err := os.Open(fn)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, chunkSize)
	for {
		n, err := f.Read(buf)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&pb.WriteFileChunk{
			BuildId:  buildId,
			Filename: filepath.Base(fn),
			Data:     buf[:n],
		}); err != nil {
			return err
		}
	}
}

// TODO: centralize dialFunc in the ratt package after re-organizing the source.
func dialFunc(target string, timeout time.Duration) (net.Conn, error) {
	log.Printf("dial %q", target)

	network := "tcp"
	if strings.HasPrefix(target, string(filepath.Separator)) {
		network = "unix"
	}
	// Local path, must be a UNIX socket
	return net.DialTimeout(network, target, timeout)
}

func (s *sbuild) build(ctx context.Context, semaphore pb.SemaphoreClient) (*buildResult, error) {
	// Acquire a build semaphore from the (possibly load-balanced) semaphore server.
	acquirestream, err := semaphore.Acquire(ctx, &pb.AcquireRequest{})
	if err != nil {
		return nil, err
	}
	acquire, err := acquirestream.Recv()
	if err != nil {
		return nil, &semaphoreAcquireError{err}
	}
	buildId := acquire.GetBuildId()

	// Connect directly to the build server to avoid being load-balanced to a
	// different server.
	buildconn, err := grpc.Dial(acquire.GetHostPort(),
		grpc.WithDialer(dialFunc),
		grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	build := pb.NewBuildClient(buildconn)
	semaphore = pb.NewSemaphoreClient(buildconn)
	baseNames := make([]string, len(s.extraDebs))
	for idx, fn := range s.extraDebs {
		baseNames[idx] = filepath.Base(fn)
		// TODO: checksum + offer first, the builder retains files in a content-addressible storage cache
		stream, err := build.WriteFile(ctx)
		if err != nil {
			return nil, err
		}
		if err := writeFile(stream, buildId, fn); err != nil {
			return nil, err
		}
		if _, err := stream.CloseAndRecv(); err != nil {
			return nil, err
		}
	}

	if _, err := build.Start(ctx, &pb.StartRequest{
		BuildId:      acquire.GetBuildId(),
		Package:      s.target,
		ExtraPackage: baseNames,
	}); err != nil {
		return nil, err
	}

	wait, err := build.Wait(ctx, &pb.WaitRequest{BuildId: buildId})
	if err != nil {
		return nil, err
	}

	// Assumption: server is trusted (no malicious tarballs).
	// Assumption: transport is secure and authenticated.
	stream, err := build.Tar(ctx, &pb.TarRequest{BuildId: buildId})
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(s.logDir, "."+s.target)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	untar := exec.Command("tar", "xz")
	untar.Dir = dir
	untar.Stderr = os.Stderr
	stdin, err := untar.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer stdin.Close()
	if err := untar.Start(); err != nil {
		return nil, err
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if _, err := stdin.Write(chunk.GetData()); err != nil {
			return nil, err
		}
	}

	dest := filepath.Join(s.logDir, s.target)
	err = os.Rename(dir, dest)
	if os.IsExist(err) {
		if err := os.RemoveAll(dest); err != nil {
			return nil, err
		}
		err = os.Rename(dir, dest) // clears outer err
		if err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}

	// TODO: specify the extra debs as move-to-cache
	if _, err := semaphore.Release(ctx, &pb.ReleaseRequest{BuildId: buildId}); err != nil {
		return nil, err
	}

	const buildArch = "amd64" // TODO
	result := &buildResult{
		logFile: filepath.Join(dest, s.target+"_"+buildArch+".build"),
	}
	if wait.GetExitStatus() != 0 {
		result.err = fmt.Errorf("exit status %d", wait.GetExitStatus())
	}
	return result, nil
}
