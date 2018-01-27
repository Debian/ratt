// Package ratt-balancer-roundrobin uses a round-robin strategy to distribute
// semaphore requests among build servers. Clients will connect directly to the
// picked build server to perform the build and release the semaphore.
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
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/naming"
	"google.golang.org/grpc/peer"

	"github.com/Debian/ratt/internal/pb"
)

var (
	port             = flag.Int("port", 12500, "listening port")
	builderPorts     = flag.String("builder_ports", "", "comma-separated list of ports (on localhost) to distribute builds to")
	socketDir        = flag.String("socket_dir", "", "path to a directory containing UNIX sockets to distribute builds to")
	concurrentBuilds = flag.Int("concurrent_builds", 32, "Maximum number of builds to allow concurrently, i.e. the sum of -concurrent_builds values of all builders. Defaults to 32 to work for a small cluster.")
)

type server struct {
	fbc    pb.SemaphoreClient
	builds int32
}

// Acquire implements pb.SemaphoreServer.
func (s *server) Acquire(req *pb.AcquireRequest, sstream pb.Semaphore_AcquireServer) error {
	cstream, err := s.fbc.Acquire(sstream.Context(), req)
	if err != nil {
		return err
	}
	for {
		c, err := cstream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		remote, ok := peer.FromContext(cstream.Context())
		if !ok {
			return fmt.Errorf("BUG: no peer in cstream.Context")
		}
		log.Printf("forwarded Semaphore.Acquire() to %s", remote.Addr.String())
		c.HostPort = remote.Addr.String()
		if err := sstream.Send(c); err != nil {
			return err
		}
	}
}

// Release implements pb.SemaphoreServer.
func (s *server) Release(ctx context.Context, req *pb.ReleaseRequest) (*pb.ReleaseReply, error) {
	return nil, fmt.Errorf("not implemented: dial AcquireReply.hostport for any request but Semaphore.Acquire")
}

// Get implements pb.ConfigurationServer.
func (s *server) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetReply, error) {
	return &pb.GetReply{
		ConcurrentBuilds: s.builds,
	}, nil
}

// resolver returns a set of addresses which never changes.
type resolver struct {
	added     bool
	addrs     []string
	socketDir string
	sockets   map[string]bool
}

// Resolve implements naming.Resolver.
func (r *resolver) Resolve(target string) (naming.Watcher, error) {
	return r, nil
}

// Next implements naming.Watcher.
func (r *resolver) Next() (upd []*naming.Update, err error) {
	if !r.added {
		updates := make([]*naming.Update, len(r.addrs))
		for idx, addr := range r.addrs {
			updates[idx] = &naming.Update{Op: naming.Add, Addr: addr}
		}
		r.added = true
		return updates, nil
	}
	if r.socketDir == "" {
		select {} // block forever, addresses will not change
	}

	// TODO(later): use inotify to sleep until we receive an update event

	fis, err := ioutil.ReadDir(r.socketDir)
	if err != nil {
		return nil, err
	}
	newSockets := make(map[string]bool, len(fis))
	for _, fi := range fis {
		if fi.Mode()&os.ModeSocket == 0 {
			continue // not a socket
		}
		newSockets[fi.Name()] = true
	}
	var updates []*naming.Update
	if !reflect.DeepEqual(r.sockets, newSockets) {
		for s := range r.sockets {
			if !newSockets[s] {
				updates = append(updates, &naming.Update{
					Op:   naming.Delete,
					Addr: filepath.Join(r.socketDir, s)})
			}
		}
		for s := range newSockets {
			if !r.sockets[s] {
				updates = append(updates, &naming.Update{
					Op:   naming.Add,
					Addr: filepath.Join(r.socketDir, s)})
			}
		}
		r.sockets = newSockets
	}
	time.Sleep(1 * time.Second) // throttle
	return updates, nil
}

// Close implements naming.Watcher.
func (r *resolver) Close() {}

func dialFunc(target string, timeout time.Duration) (net.Conn, error) {
	log.Printf("dial %q with timeout %v", target, timeout)

	network := "tcp"
	if strings.HasPrefix(target, string(filepath.Separator)) {
		network = "unix"
	}
	// Local path, must be a UNIX socket
	return net.DialTimeout(network, target, timeout)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	flag.Parse()

	// TODO(later): implement systemd socket activation. that way, we can have
	// ratt-balancer-roundrobin.path activate ratt-balancer-roundrobin.socket,
	// so that remote builders are not started until necessary.

	var addrs []string
	for _, part := range strings.Split(*builderPorts, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		port, err := strconv.ParseInt(part, 0, 64)
		if err != nil {
			log.Fatalf("Invalid port: %q: %v", part, err)
		}
		addrs = append(addrs, fmt.Sprintf("localhost:%d", port))
	}
	if len(addrs) == 0 && *socketDir == "" {
		log.Fatalf("Specify at least one address in -builder_ports, e.g. -builder_ports=12311, or specify -socket_dir")
	}

	conn, err := grpc.Dial("", // our resolver ignores target
		grpc.WithBalancer(grpc.RoundRobin(&resolver{
			addrs:     addrs,
			socketDir: *socketDir,
			sockets:   make(map[string]bool),
		})),
		grpc.WithDialer(dialFunc),
		grpc.WithBackoffMaxDelay(10*time.Second),
		grpc.WithInsecure())
	if err != nil {
		log.Fatal(err)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", *port))
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		fbc:    pb.NewSemaphoreClient(conn),
		builds: int32(*concurrentBuilds),
	}

	grpcServer := grpc.NewServer()
	pb.RegisterConfigurationServer(grpcServer, s)
	pb.RegisterSemaphoreServer(grpcServer, s)
	log.Fatal(grpcServer.Serve(ln))
}
