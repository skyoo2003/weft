// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/query"
)

// checkCmd verifies a committed directory.
//
// engine.Scrub is neither a repair nor a delete: it reads a directory and
// reports what does not add up — a document whose stored length disagrees with
// its postings, a segment that will not decode. There is no reclaiming half of
// anything in weft, so the answer is either "no damage found" or a description
// of the damage, and what to do about the second is a full re-index.
func checkCmd(args []string, stdout, stderr io.Writer) error {
	var data string
	_, err := parseFlags(cmdCheck, args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&data, "data", "", "directory holding the index to verify")
	})
	if err != nil {
		return err
	}
	if data == "" {
		return badUsage("-data names the directory to verify, and it is required")
	}

	if err := engine.Scrub(data); err != nil {
		return fmt.Errorf("%s: %w", data, err)
	}
	outf(stdout, "no damage found in %s\n", data)
	return nil
}

// encodeCmd prints the term a value is indexed under.
//
// query.Range compares bytes. A caller writing a numeric or date bound into a
// query string has to write it in the encoding the indexing side used, and
// pkg/query says so without giving anyone a way to produce one: *write
// EncodeInt's output into the query, or build the Range scorer directly*. The
// second half of that sentence needs a Go program. This is the first half.
//
//	weft encode -int 42
//	weft search -data ./ix -q "price:[$(weft encode -int 0) TO $(weft encode -int 100)]"
func encodeCmd(args []string, stdout, stderr io.Writer) error {
	var n, ts string
	fs, err := parseFlags(cmdEncode, args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&n, "int", "", "an integer to encode as an order-preserving term")
		fs.StringVar(&ts, "time", "", "an RFC 3339 time to encode as an order-preserving term")
	})
	if err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	switch {
	case set["int"] && set["time"]:
		return badUsage("-int and -time encode different things and this command prints one term; " +
			"run it twice")
	case set["int"]:
		v, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return badUsage("-int %q is not an integer", n)
		}
		outln(stdout, query.EncodeInt(v))
		return nil
	case set["time"]:
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return badUsage("-time %q is not an RFC 3339 time: %v", ts, err)
		}
		outln(stdout, query.EncodeTime(t))
		return nil
	default:
		return badUsage("nothing to encode: give -int an integer or -time an RFC 3339 time")
	}
}
