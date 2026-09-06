// SPDX-License-Identifier: Apache-2.0

// Command weftg serves weft's native search surface over gRPC.
//
// It reads the same data directory weftd reads and answers the same requests
// docs/API.md documents, in a second encoding. Search only: writing goes through
// weftd, because one writer per index is the invariant an index holds and two
// processes over one directory would each believe they were it.
//
//	cd grpc && go run ./cmd/weftg -data ../.weftd-data
//
// # Read this before exposing it
//
// weft is not usable in production and this does not change that. Sustained load
// collapses at 27 queries a second rather than degrading, and there is no
// authentication and no TLS here. It binds loopback for that reason.
// docs/STATUS.md and docs/LIMITATIONS.md are the full account.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	weftgrpc "github.com/skyoo2003/weft/grpc"
	"github.com/skyoo2003/weft/grpc/weftpb"
	"github.com/skyoo2003/weft/internal/opensearch"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9201",
		"address to listen on; loopback by default because there is no authentication and no TLS")
	data := flag.String("data", ".weftd-data",
		"directory holding one subdirectory per index — the same one weftd reads")
	flag.Parse()

	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "weftg takes no arguments, and %q is not a flag\n", flag.Arg(0))
		os.Exit(2)
	}

	log.SetFlags(log.Ltime)
	if err := run(*addr, *data); err != nil {
		log.Fatal(err)
	}
}

func run(addr, data string) error {
	// A directory that will not open is fatal rather than skipped, for weftd's
	// reason: a server missing one of its indexes answers NotFound for it, and a
	// client cannot tell that from an index that was never created.
	reg, err := opensearch.OpenRegistry(data)
	if err != nil {
		return err
	}
	defer reg.Close() //nolint:errcheck // the shutdown path below reports what it can

	// The signal context is taken before the listener, so a Ctrl-C during startup
	// is caught by the handler that catches one during service.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Listen before printing, so a port in the banner is a port that is bound.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	banner(ln.Addr().String(), data, reg.Names())

	srv := grpc.NewServer()
	weftpb.RegisterWeftServer(srv, weftgrpc.New(reg))

	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Print("shutting down; in-flight requests finish first")
		// GracefulStop and not Stop: a search in flight is cheap to wait for, and
		// there is no commit here to be cut short — this surface does not write.
		srv.GracefulStop()
		return reg.Close()
	}
}

// banner says what this is and what it is not, every time it starts.
//
// Not decoration, and the same argument weftd's makes: docs/STATUS.md says weft
// is not usable in production, and a network surface is the thing that makes that
// easy to forget, so the warning travels with the process.
func banner(addr, data string, indexes []string) {
	log.Printf("weftg listening on %s, data in %s, %d index(es): %v", addr, data, len(indexes), indexes)
	log.Printf("  search only — writes go through weftd, because one writer per index is the invariant")
	log.Printf("  NOT FOR PRODUCTION: sustained load collapses at 27 q/s, and there is no")
	log.Printf("  authentication and no TLS here. docs/STATUS.md and docs/LIMITATIONS.md say the rest.")
}
