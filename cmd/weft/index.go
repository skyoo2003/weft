// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
)

// maxLine is the largest document line accepted. bufio.Scanner's own default is
// 64 KiB, which a paper abstract with a 768-dimension vector beside it passes
// without trying — and the failure mode is a scan that stops early and reports
// success, because Scan returns false for both "done" and "too long" and only
// Err can tell them apart.
const maxLine = 16 << 20

// jsonDoc is engine.Document in the spelling a JSON line uses.
//
// A separate type rather than tags on the library's own, because Document is
// pkg/ and this is a cmd/: adding `json:"..."` to it would make these wire names
// part of the library's contract, and D-025's line is that a command changes
// nothing under pkg/. It also lets Time be a string here, which is what a shell
// can produce.
type jsonDoc struct {
	Key    string    `json:"key"`
	Text   string    `json:"text"`
	Vector []float32 `json:"vector"`
	Links  []string  `json:"links"`
	Time   string    `json:"time"`
	Fields []struct {
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"fields"`
}

// document converts one line into what Add takes.
func (j jsonDoc) document() (engine.Document, error) {
	d := engine.Document{
		Key:    j.Key,
		Text:   j.Text,
		Vector: j.Vector,
		Links:  j.Links,
	}
	if j.Time != "" {
		t, err := time.Parse(time.RFC3339, j.Time)
		if err != nil {
			return engine.Document{}, fmt.Errorf("time %q: %w", j.Time, err)
		}
		d.Time = t
	}
	for _, f := range j.Fields {
		d.Fields = append(d.Fields, engine.Field{Name: f.Name, Text: f.Text})
	}
	return d, nil
}

// indexCmd reads documents from stdin and commits them.
func indexCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var data, tokName, del string
	var merge bool
	_, err := parseFlags(cmdIndex, args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&data, "data", "", "directory holding the index; created if it is missing or empty")
		fs.StringVar(&tokName, "tokenizer", defaultTokenizer,
			"tokenizer to index with: "+strings.Join(tokenizerNames(), ", ")+
				". An index opens only under the tokenizer it was committed with")
		fs.StringVar(&del, "delete", "",
			"comma-separated document keys to delete, applied before stdin is read")
		fs.BoolVar(&merge, "merge", false, "merge segments before committing")
	})
	if err != nil {
		return err
	}

	if data == "" {
		return badUsage("-data names the directory to write the index to, and it is required: " +
			"an index built and written nowhere is a process that did nothing")
	}
	tok, ok := tokenizers[tokName]
	if !ok {
		return badUsage("unknown tokenizer %q; the choices are %s", tokName, strings.Join(tokenizerNames(), ", "))
	}

	// The signal context covers the commit, which is the long part: milestone 9
	// measured 11 seconds on 20,000 documents. Commit already promises that a
	// cancelled one leaves the previous generation on disk, so this is a Ctrl-C
	// that costs the run's writes rather than the index.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ix, err := openOrCreate(data, engine.WithTokenizer(tok))
	if err != nil {
		return err
	}
	defer ix.Close() //nolint:errcheck // the commit below is what reports a write failure

	var deleted int
	for _, key := range splitList(del) {
		if ix.Delete(key) {
			deleted++
			continue
		}
		// Not an error. A delete of a key that is not there has already achieved
		// what it was asked for, and a script cleaning up after itself should not
		// have to know which keys it managed to write last time.
		outf(stderr, "weft: no document keyed %q to delete\n", key)
	}

	added, updated, err := read(stdin, ix)
	if err != nil {
		return err
	}

	if merge {
		if err := ix.Merge(); err != nil {
			return fmt.Errorf("merge: %w", err)
		}
		outln(stdout, "merged")
	}
	if err := ix.Commit(ctx, data); err != nil {
		return fmt.Errorf("commit to %s: %w", data, err)
	}

	docs, avg := ix.Stats()
	outf(stdout, "added %d, updated %d, deleted %d; %d documents, average length %.1f\n",
		added, updated, deleted, docs, avg)
	outf(stdout, "committed to %s\n", data)
	return nil
}

// read consumes the JSON lines and applies each to the index.
//
// Add for a key the index does not hold and Update for one it does, rather than
// Add always: Add refuses a duplicate key with ErrDuplicateKey, so re-indexing a
// corpus sharing one document with the last run would fail on that document and
// take the whole run down with it.
func read(stdin io.Reader, ix *engine.Index) (added, updated int, err error) {
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)

	for line := 1; sc.Scan(); line++ {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var j jsonDoc
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			return added, updated, fmt.Errorf("line %d: %w", line, err)
		}
		d, err := j.document()
		if err != nil {
			return added, updated, fmt.Errorf("line %d: %w", line, err)
		}
		if _, live := ix.Resolve(d.Key); live {
			if _, err := ix.Update(d); err != nil {
				return added, updated, fmt.Errorf("line %d: update %q: %w", line, d.Key, err)
			}
			updated++
			continue
		}
		if _, err := ix.Add(d); err != nil {
			return added, updated, fmt.Errorf("line %d: add %q: %w", line, d.Key, err)
		}
		added++
	}
	if err := sc.Err(); err != nil {
		return added, updated, fmt.Errorf("read stdin: %w", err)
	}
	return added, updated, nil
}

// openOrCreate opens the index in dir, or starts an empty one where there is
// nothing yet.
//
// A missing or empty directory is a new index; anything else is opened, and an
// open that fails is reported rather than stepped over. Starting fresh on top of
// a directory engine could not read is how a run comes to report "added 3" over
// a corpus it has just made unreachable.
func openOrCreate(dir string, opts ...engine.Option) (*engine.Index, error) {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
		return engine.New(opts...), nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", dir, err)
	case len(entries) == 0:
		return engine.New(opts...), nil
	default:
		return engine.Open(dir, opts...)
	}
}

// splitList turns a comma-separated flag value into its parts, dropping empties
// so a trailing comma is not a request to act on "".
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
