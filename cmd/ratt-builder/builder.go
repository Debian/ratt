// Binary ratt-builder provides a gRPC service to build Debian packages.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/Debian/ratt/internal/pb"
	"google.golang.org/grpc"
)

var (
	cacheDir         = flag.String("cache_dir", "/var/cache/ratt-builder", "Directory in which to build packages (can safely be deleted)")
	concurrentBuilds = flag.Int("concurrent_builds", runtime.NumCPU(), "Maximum number of builds to allow concurrently. Defaults to the number of cores.")
)

// relativeTo returns nil if dir is relative to base after resolving its absolute path.
func relativeTo(base, dir string) error {
	abs, err := filepath.Abs(filepath.Join(base, dir))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return err
	}
	if rel != dir {
		return fmt.Errorf("invalid file name %q: outside of the cache directory", dir)
	}
	return nil
}

type buildId string

type server struct {
	dir string

	mu           sync.Mutex
	builds       map[buildId]chan struct{}
	writtenFiles map[buildId][]string // WriteFile() filenames
	cmds         map[buildId]*exec.Cmd
}

func (s *server) buildDir(buildID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.builds[buildId(buildID)]; !ok {
		return "", fmt.Errorf("%q is not a valid build id", buildID)
	}
	return filepath.Join(s.dir, buildID), nil
}

func (s *server) acquireSemaphore() (_ buildId, done chan struct{}, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.builds) >= *concurrentBuilds {
		return "", nil, fmt.Errorf("overloaded: maximum concurrent builds reached")
	}

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return "", nil, err
	}
	name, err := ioutil.TempDir(s.dir, "build")
	if err != nil {
		return "", nil, err
	}
	// TODO: record a file called .ratt.lock, containing the PID and UNIX start time of the lock-owning process.
	// TODO: clean up on startup, i.e. delete all build dirs for which the .ratt.lock file refers to an invalid PID or to a PID which was not started at the same timestamp (against accidental PID reuse)
	ch := make(chan struct{})
	id := buildId(filepath.Base(name))
	s.builds[id] = ch
	log.Printf("semaphore %q acquired", id)
	return id, ch, nil
}

func (s *server) Acquire(req *pb.AcquireRequest, stream pb.Semaphore_AcquireServer) error {
	id, done, err := s.acquireSemaphore()
	if err != nil {
		return err
	}

	if err := stream.Send(&pb.AcquireReply{
		BuildId:  string(id),
		HostPort: fmt.Sprintf("localhost:%d", 12311),
	}); err != nil {
		return err
	}

	select {
	case <-done: // Keep this RPC pending until Release() is called.
	case <-stream.Context().Done(): // RPC cancelled
	}

	return nil
}

func (s *server) Release(ctx context.Context, req *pb.ReleaseRequest) (*pb.ReleaseReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := buildId(req.GetBuildId())
	if _, ok := s.builds[id]; !ok {
		return nil, fmt.Errorf("%q is not a valid build id", req.GetBuildId())
	}
	close(s.builds[id])
	delete(s.builds, id)
	log.Printf("semaphore %q released", id)
	return &pb.ReleaseReply{}, nil
}

func (s *server) WriteFile(stream pb.Build_WriteFileServer) error {
	var f *os.File
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			if err := f.Close(); err != nil {
				return err
			}
			return stream.SendAndClose(&pb.WriteFileReply{})
		}
		if err != nil {
			return err
		}
		if f == nil {
			buildDir, err := s.buildDir(c.GetBuildId())
			if err != nil {
				return err
			}
			if err := relativeTo(buildDir, c.GetFilename()); err != nil {
				return err
			}
			f, err = os.Create(filepath.Join(buildDir, c.GetFilename()))
			if err != nil {
				return err
			}
			defer f.Close()
			id := buildId(c.GetBuildId())
			s.mu.Lock()
			s.writtenFiles[id] = append(s.writtenFiles[id], c.GetFilename())
			s.mu.Unlock()
		}
		if _, err := f.Write(c.Data); err != nil {
			return err
		}
	}
}

func (s *server) Start(ctx context.Context, req *pb.StartRequest) (*pb.StartReply, error) {
	// Validate the build request:
	buildDir, err := s.buildDir(req.GetBuildId())
	if err != nil {
		return nil, err
	}

	args := []string{"--arch-all"}

	if strings.TrimSpace(req.GetPackage()) == "" {
		return nil, fmt.Errorf("no package to build specified")
	}

	dist := req.GetDistribution()
	if dist == "" {
		dist = "sid"
	}

	args = append(args, "--dist="+dist)

	for _, fn := range req.GetExtraPackage() {
		if err := relativeTo(buildDir, fn); err != nil {
			return nil, err
		}
		args = append(args, "--extra-package="+fn)
	}

	// Execute build request:
	stdout, err := os.Create(filepath.Join(buildDir, "STDOUT"))
	if err != nil {
		return nil, err
	}
	defer stdout.Close()

	stderr, err := os.Create(filepath.Join(buildDir, "STDERR"))
	if err != nil {
		return nil, err
	}
	defer stderr.Close()

	cmd := exec.Command("sbuild", append(args, req.GetPackage())...)
	cmd.Dir = buildDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cmds[buildId(req.GetBuildId())] = cmd
	s.mu.Unlock()
	return &pb.StartReply{}, nil
}

func (s *server) Wait(ctx context.Context, req *pb.WaitRequest) (*pb.WaitReply, error) {
	s.mu.Lock()
	cmd, ok := s.cmds[buildId(req.GetBuildId())]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("build with id %q not found", req.GetBuildId())
	}
	err := cmd.Wait()
	log.Printf("[%s] wait: %v", req.GetBuildId(), err)
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			return &pb.WaitReply{
				ExitStatus: uint32(ws.ExitStatus()),
			}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return &pb.WaitReply{ExitStatus: 0}, nil
}

func (s *server) Tar(req *pb.TarRequest, stream pb.Build_TarServer) error {
	const chunkSize = 3 * 1024 * 1024 // under the 4 MiB gRPC message size limit

	buildDir, err := s.buildDir(req.GetBuildId())
	if err != nil {
		return err
	}

	// TODO: this is linux-only. replace with a go tar implementation once finding/writing one
	s.mu.Lock()
	writtenFiles := s.writtenFiles[buildId(req.GetBuildId())]
	args := make([]string, len(writtenFiles))
	for idx, fn := range writtenFiles {
		args[idx] = "--exclude=./" + fn
	}
	s.mu.Unlock()

	tar := exec.CommandContext(stream.Context(), "tar", append(append([]string{"cz"}, args...), ".")...)
	tar.Dir = buildDir
	tar.Stderr = os.Stderr
	stdout, err := tar.StdoutPipe()
	if err != nil {
		return err
	}
	defer stdout.Close()
	if err := tar.Start(); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	for {
		n, err := stdout.Read(buf)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&pb.TarChunk{Data: buf[:n]}); err != nil {
			return err
		}
	}
}

func (s *server) Clean(ctx context.Context, req *pb.CleanRequest) (*pb.CleanReply, error) {
	return nil, fmt.Errorf("not yet implemented")
}

// Get implements pb.ConfigurationServer
func (s *server) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetReply, error) {
	return &pb.GetReply{
		ConcurrentBuilds: int32(*concurrentBuilds),
	}, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	flag.Parse()

	// TODO(later): implement systemd socket activation. that way, we can have
	// ratt-builder.socket, so that the builder is not started until necessary.

	if *concurrentBuilds < 1 {
		log.Fatalf("-concurrent_builds=%d must be at least 1", *concurrentBuilds)
	}

	ln, err := net.Listen("tcp", "localhost:12311")
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		dir:          *cacheDir,
		builds:       make(map[buildId]chan struct{}),
		writtenFiles: make(map[buildId][]string),
		cmds:         make(map[buildId]*exec.Cmd),
	}

	grpcServer := grpc.NewServer()
	pb.RegisterBuildServer(grpcServer, s)
	pb.RegisterSemaphoreServer(grpcServer, s)
	log.Fatal(grpcServer.Serve(ln))
}
