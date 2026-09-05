// SPDX-License-Identifier: Apache-2.0

// Command weftd serves weft over a subset of the OpenSearch REST API.
//
// It exists so that trying weft does not require writing a Go program. What it
// is not is a second product: the library is the product, this is a cmd/, and
// milestone 24 changes zero lines under pkg/ — see docs/DECISIONS.md D-025.
//
//	go run ./cmd/weftd -data ./weft-data
//	curl -XPUT localhost:9200/papers
//	curl -XPUT 'localhost:9200/papers/_doc/1?refresh=true' \
//	  -H 'Content-Type: application/json' -d '{"text":"reciprocal rank fusion"}'
//	curl -XPOST localhost:9200/papers/_search \
//	  -H 'Content-Type: application/json' -d '{"query":{"match":{"text":"fusion"}}}'
//
// GET / reports OpenSearch 2.19.0, which is untrue and is the only untrue thing
// it says; a query it cannot express is a 400 or a 501 and never an empty
// success. D-026 is the argument.
//
// # Read this before exposing it
//
// weft is not usable in production and weftd does not change that. Sustained
// load collapses at 27 queries a second rather than degrading, a commit holds
// the writer for 11 seconds on a 20,000-document batch, and there is no
// authentication and no TLS here. It binds to loopback by default for that
// reason. docs/STATUS.md and docs/LIMITATIONS.md are the full account.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/skyoo2003/weft/internal/opensearch"
)

// shutdownGrace is how long an in-flight request has to finish after a signal.
// Longer than a search and shorter than a commit, which is the honest middle: a
// commit still running gets cancelled through the request context and leaves the
// previous generation on disk, because that is what Commit already promises.
const shutdownGrace = 10 * time.Second

func main() {
	addr := flag.String("addr", "127.0.0.1:9200",
		"address to listen on; loopback by default because there is no authentication and no TLS")
	data := flag.String("data", ".weftd-data",
		"directory holding one subdirectory per index")
	maxBody := flag.Int64("max-body", opensearch.DefaultMaxBody,
		"largest request body accepted, in bytes")
	flag.Parse()

	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "weftd takes no arguments, and %q is not a flag\n", flag.Arg(0))
		os.Exit(2)
	}
	if *maxBody <= 0 {
		fmt.Fprintf(os.Stderr, "-max-body must be positive, got %d\n", *maxBody)
		os.Exit(2)
	}

	log.SetFlags(log.Ltime)
	if err := run(*addr, *data, *maxBody); err != nil {
		log.Fatal(err)
	}
}

func run(addr, data string, maxBody int64) error {
	reg, err := opensearch.OpenRegistry(data)
	if err != nil {
		// A directory that will not open is fatal rather than skipped: a server
		// missing one of its indexes answers 404 for it, and a client cannot
		// tell that from an index that was never created.
		return err
	}
	defer reg.Close() //nolint:errcheck // the shutdown path below reports what it can

	// Listen before printing, so a port in the banner is a port that is bound.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	banner(ln.Addr().String(), data, reg.Names())

	srv := &http.Server{
		Handler: opensearch.NewServer(reg, maxBody),
		// A slow client should not hold a connection open for free while it
		// dribbles out headers.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(ln) }()

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("shutting down; in-flight requests have %s", shutdownGrace)
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		// Registry.Close stops every writer goroutine and releases every
		// mapping. Uncommitted writes are lost, which is what an index with no
		// commit has always meant in weft.
		return reg.Close()
	}
}

// banner says what this is and what it is not, every time it starts.
//
// Not decoration. docs/STATUS.md says weft is not usable in production, and
// weftd is the first thing that makes that easy to forget, so the warning
// travels with the process rather than living only in a file nobody opened.
func banner(addr, data string, indexes []string) {
	log.Printf("weftd listening on %s, data in %s, %d index(es): %v", addr, data, len(indexes), indexes)
	log.Printf("  announcing OpenSearch %s — weft is not OpenSearch; see docs/DECISIONS.md D-026", opensearch.Version)
	log.Printf("  NOT FOR PRODUCTION: sustained load collapses at 27 q/s, a commit holds the writer")
	log.Printf("  for 11 s on 20,000 documents, and there is no authentication and no TLS here.")
	log.Printf("  docs/STATUS.md and docs/LIMITATIONS.md are the full account.")
}
