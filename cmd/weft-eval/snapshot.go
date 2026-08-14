package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The downloaded inputs every published number was produced from, pinned by size and
// content hash.
//
// docs/EVAL.md section 5.1 names a release and a date for each of these, and until now
// that was prose: nothing compared the file on disk to it. Two ways that mattered, both
// of which produce a complete-looking run rather than an error.
//
// A metadata.csv truncated by an interrupted 1.6 GB download still parses. Its header is
// intact and its rows are valid, so the join succeeds on whatever prefix arrived, and
// prepare records every corpus document the file never mentioned as asked-and-unjoinable
// — a tombstone build reads as full coverage. The measured shape of the real release is
// what makes that indistinguishable from success: all 165,192 corpus keys present in it
// carry an identifier, so the 6,140 documents without one are simply absent from the
// release, and absence is therefore normal rather than a signal.
//
// A qrels file truncated at a row boundary is worse, because it is silent in both
// directions: bufio.Scanner reports a clean EOF, the reader accepts a valid prefix, and
// the queries whose judgments were cut evaluate against a smaller ideal ranking. Every
// arm then scores higher than it should, by an amount nothing reports.
//
// Only the downloads are here. s2.jsonl and query-vectors.jsonl are generated, so their
// bytes depend on API responses and on a local model rather than on a published
// snapshot; what covers those is prepare's model tally, build's coverage gate and
// loadQueries' text pairing.
var snapshot = []struct {
	name string
	size int64
	sha  string
}{
	{corpusFile, 221370065, "aded69896598665082316d00df487d22212cc5a8611db05db34eb40d04ed00d7"},
	{queriesFile, 16552, "78f4b76bfef251a0baf37a6cea5c4ba8376cb4536c34635709aaa4a2783b208c"},
	{qrelsFile, 980831, "10669ab7d526cb04f52079139fd88c3d467a0776441b046567f540582798982b"},
	{metadataFile, 1648942196, "ec2e3c55f698f01f2faa2bf7d80fd72d488bb762270565c9967b54f82bf31ae5"},
}

// snapshotFlag registers the opt-out. The wording lives in one place because this is the
// flag that decides whether a number is publishable.
func snapshotFlag(fs *flag.FlagSet, any *bool) {
	fs.BoolVar(any, "any-snapshot", false,
		"skip the size and hash check on the downloaded inputs (for a different corpus or "+
			"release; numbers measured that way are not the ones docs/EVAL.md publishes)")
}

// verifySnapshot checks the named files under dir against the pinned release.
//
// Size is compared before the hash, so the failure this exists for — a truncated
// download — costs a stat rather than a read of 1.6 GB. A name missing from the table is
// a programming error rather than a bad input, and is reported as one.
func verifySnapshot(dir string, names ...string) error {
	for _, name := range names {
		var wantSize int64
		var wantSHA string
		found := false
		for _, s := range snapshot {
			if s.name == name {
				wantSize, wantSHA, found = s.size, s.sha, true
				break
			}
		}
		if !found {
			return fmt.Errorf("verifySnapshot: %s is not in the pinned snapshot table", name)
		}

		path := filepath.Join(dir, name)
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("snapshot check: %w (docs/EVAL.md section 5.1 lists the downloads)", err)
		}
		if fi.Size() != wantSize {
			return fmt.Errorf("%s is %d bytes, the release documented in docs/EVAL.md section 5.1 is "+
				"%d; a truncated or different file parses cleanly and produces a complete-looking run, "+
				"so it is refused here (pass -any-snapshot to measure something else deliberately)",
				path, fi.Size(), wantSize)
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("snapshot check: %w", err)
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("hash %s: %w", path, err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
			return fmt.Errorf("%s has the documented size but sha256 %s, want %s — same length, "+
				"different bytes (pass -any-snapshot to measure something else deliberately)",
				path, got, wantSHA)
		}
	}
	return nil
}
