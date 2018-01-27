// Binary grpcbuild triggers a build via gRPC (useful to verify your setup).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Debian/ratt/internal/pb"
	"google.golang.org/grpc"
)

var (
	extraPackages = flag.String("extra_packages", "", "comma-separated list of extra .deb files to install in the build")
)

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

func logic(ctx context.Context) error {
	conn, err := grpc.Dial("localhost:12500", grpc.WithInsecure())
	if err != nil {
		return err
	}
	defer conn.Close()
	configuration := pb.NewConfigurationClient(conn)
	semaphore := pb.NewSemaphoreClient(conn)

	config, err := configuration.Get(ctx, &pb.GetRequest{})
	if err != nil {
		return err
	}
	log.Printf("builder config %+v", config)

	acquirestream, err := semaphore.Acquire(ctx, &pb.AcquireRequest{})
	if err != nil {
		return err
	}
	s, err := acquirestream.Recv()
	if err != nil {
		return fmt.Errorf("acquiring build semaphore: %v", err)
	}
	buildId := s.GetBuildId()
	log.Printf("build id %q on %q", buildId, s.GetHostPort())

	buildconn, err := grpc.Dial(s.GetHostPort(), grpc.WithInsecure())
	if err != nil {
		return err
	}
	build := pb.NewBuildClient(buildconn)
	semaphore = pb.NewSemaphoreClient(buildconn)
	extraPackage := strings.Split(*extraPackages, ",")
	baseNames := make([]string, len(extraPackage))
	for idx, fn := range extraPackage {
		baseNames[idx] = filepath.Base(fn)
		stream, err := build.WriteFile(ctx)
		if err != nil {
			return err
		}
		if err := writeFile(stream, buildId, fn); err != nil {
			return err
		}
		wf, err := stream.CloseAndRecv()
		if err != nil {
			return err
		}
		log.Printf("write file %q: %+v", fn, wf)
	}

	b, err := build.Start(ctx, &pb.StartRequest{
		BuildId:      s.GetBuildId(),
		Package:      "hello_2.10-1",
		ExtraPackage: baseNames,
	})
	if err != nil {
		return err
	}
	log.Printf("build %+v started", b)

	wait, err := build.Wait(ctx, &pb.WaitRequest{BuildId: buildId})
	if err != nil {
		return err
	}
	log.Printf("build done: %+v", wait)

	// Assumption: server is trusted (no malicious tarballs).
	// Assumption: transport is secure and authenticated.
	stream, err := build.Tar(ctx, &pb.TarRequest{BuildId: buildId})
	if err != nil {
		return err
	}

	dir := filepath.Join("buildlogs", ".hello_2.10-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	untar := exec.Command("tar", "xz")
	untar.Dir = dir
	untar.Stderr = os.Stderr
	stdin, err := untar.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	if err := untar.Start(); err != nil {
		return err
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if _, err := stdin.Write(chunk.GetData()); err != nil {
			return err
		}
	}

	dest := filepath.Join("buildlogs", "hello_2.10-1")
	err = os.Rename(dir, dest)
	if os.IsExist(err) {
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		err = os.Rename(dir, dest) // clears outer err
		if err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}

	release, err := semaphore.Release(ctx, &pb.ReleaseRequest{BuildId: buildId})
	if err != nil {
		return err
	}
	log.Printf("release: %+v", release)
	// TODO: clean up the buildid with file exceptions unless this is the last build
	// TODO: can re-cycle the buildid here

	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	flag.Parse()

	ctx := context.Background()
	if err := logic(ctx); err != nil {
		log.Fatal(err)
	}
}
