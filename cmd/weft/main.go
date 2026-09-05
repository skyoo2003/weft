// SPDX-License-Identifier: Apache-2.0

// Command weft is the command line in front of the library.
//
// The library is the product and this is a cmd/ — docs/DECISIONS.md D-025 — so
// nothing here is a capability weft has and a `go get` user does not. What it is
// for is the other direction: reaching pkg/ from a shell, without writing a Go
// program first.
//
//	weft index -data ./ix < corpus.jsonl
//	weft search -data ./ix -q '+covid "airborne transmission"' -scorers vector,recency -breakdown
//
// Each subcommand takes its input from flags alone. A leftover positional
// argument is refused rather than ignored, because flag stops parsing at the
// first non-flag argument and says nothing about it, so a typo would silently
// leave every flag after it at its default.
//
// For weft embedded in a Go program, see ./examples: basic is the smallest one,
// breakdown prints what fusion did, weights shows a stream being discounted, and
// sparse is about the documents a scorer cannot see.
//
// # Read this before pointing it at anything that matters
//
// weft is not usable in production. docs/STATUS.md and docs/LIMITATIONS.md are
// the full account.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// Subcommand names. Written once because two places have to agree on each: the
// switch in run, and the FlagSet the subcommand builds — the FlagSet name is
// what -h prints and what a leftover-argument error quotes back, so a drift
// between the two tells an operator about a command they did not run.
const (
	cmdIndex   = "index"
	cmdSearch  = "search"
	cmdInspect = "inspect"
	cmdCheck   = "check"
	cmdEncode  = "encode"
)

// subcommands is that same list in the order the help prints them, and is what
// TestUsageListsEverySubcommand reads.
var subcommands = []string{cmdIndex, cmdSearch, cmdInspect, cmdCheck, cmdEncode}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is main with its arguments and streams passed in, so a test can drive the
// whole command without a subprocess and without os.Exit.
//
// The exit code carries the difference between the two kinds of failure: 2 is a
// command a caller fixes by retyping it, and 1 is one where retyping is not the
// problem. Both print to stderr.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		// Help, not a session. `weft` alone used to index a built-in corpus and
		// read queries from stdin, which is a thing to demonstrate rather than a
		// thing to be — that demo lives in ./examples now.
		usage(stderr)
		return 2
	}

	name, rest := args[0], args[1:]
	switch name {
	case cmdIndex:
		return report(stderr, indexCmd(rest, stdin, stdout, stderr))
	case cmdSearch:
		return report(stderr, searchCmd(rest, stdout, stderr))
	case cmdInspect:
		return report(stderr, inspectCmd(rest, stdout, stderr))
	case cmdCheck:
		return report(stderr, checkCmd(rest, stdout, stderr))
	case cmdEncode:
		return report(stderr, encodeCmd(rest, stdout, stderr))
	case "help", "-h", "--help":
		// Asking for the help is a request that succeeded; being handed it after
		// typing nothing is a diagnostic. Different streams, different codes.
		usage(stdout)
		return 0
	default:
		outf(stderr, "weft: unknown subcommand %q\n\n", name)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	outs(w, `usage: weft <index|search|inspect|check|encode> [flags]

  index    read documents as JSON lines on stdin, then commit them to -data.
           One object per line: key, text, vector, links, time, fields. Deletes
           and a segment merge happen in the same run, before the commit.
  search   rank an index. Streams come from -scorers and from -q, weft's own
           query string, and -weights discounts them by position. -breakdown
           prints each scorer's own rank beside the fused one, which is how you
           see that the fused order is nobody's order.
  inspect  what the index says about itself: stats, one document, a term's
           postings, the term space, the nearest vectors, the link graph, and
           -analyze, which tokenizes text with no index at all.
  check    verify a committed directory. Reports damage; repairs nothing,
           because a re-index is the only repair weft has.
  encode   print the order-preserving term an integer or a time is indexed
           under, so a range bound can be written into -q by hand.

Run any subcommand with -h for its flags. weft is not usable in production;
docs/STATUS.md is the account.
`)
}

// outf, outln and outs are Fprintf, Fprintln and Fprint with the error dropped.
//
// The same shape cmd/weft-eval uses, for the same reason: a write to stdout that
// fails has already lost the thing the caller wanted to see, and there is nowhere
// left to report it — stderr is often the same pipe, so the second write fails
// too. The exit code still carries whether the work succeeded. What a closed pipe
// changes is only whether anyone was reading.
func outf(w io.Writer, format string, a ...any) {
	fmt.Fprintf(w, format, a...) //nolint:errcheck // see above
}

func outln(w io.Writer, a ...any) {
	fmt.Fprintln(w, a...) //nolint:errcheck // see outf
}

func outs(w io.Writer, s string) {
	fmt.Fprint(w, s) //nolint:errcheck // see outf
}

// usageError marks a failure a caller fixes by retyping the command.
type usageError struct{ error }

// badUsage builds one.
func badUsage(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

// report prints a subcommand's error and turns it into an exit code.
func report(stderr io.Writer, err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		// The FlagSet has already printed the flags. Asking for them is not a
		// failure, exactly as `weft help` is not.
		return 0
	}
	outf(stderr, "weft: %v\n", err)
	var ue usageError
	if errors.As(err, &ue) {
		return 2
	}
	return 1
}

// parseFlags builds a subcommand's FlagSet, registers its flags and parses args.
//
// ContinueOnError rather than the ExitOnError cmd/weft-eval uses: a flag package
// that calls os.Exit takes the test process with it, and the exit code is the
// thing being asserted.
//
// A leftover positional argument is refused for the reason weft-eval refuses it.
// flag stops at the first non-flag argument and says nothing, so `weft index
// typo -data ./ix` would leave -data empty and report a missing flag the caller
// can see they typed.
func parseFlags(name string, args []string, out io.Writer, register func(*flag.FlagSet)) (*flag.FlagSet, error) {
	fs := flag.NewFlagSet("weft "+name, flag.ContinueOnError)
	fs.SetOutput(out)
	register(fs)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, usageError{err}
	}
	if fs.NArg() > 0 {
		return nil, badUsage("%s takes no arguments, and %q is not a flag: parsing stopped there, "+
			"so every flag after it was ignored and left at its default", name, fs.Arg(0))
	}
	return fs, nil
}
