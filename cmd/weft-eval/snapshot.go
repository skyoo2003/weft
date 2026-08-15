package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// pinned returns the size and hash the table fixes for name. A name that is not in it
// is a programming error rather than a bad input, and callers report it as one.
func pinned(name string) (int64, string, bool) {
	for _, s := range snapshot {
		if s.name == name {
			return s.size, s.sha, true
		}
	}
	return 0, "", false
}

// sha256File hashes a file streaming, which is the only way it can be asked of a
// 1.6 GB one.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifySnapshot checks the named files under dir against the pinned release.
//
// Size is compared before the hash, so the failure this exists for — a truncated
// download — costs a stat rather than a read of 1.6 GB.
func verifySnapshot(dir string, names ...string) error {
	for _, name := range names {
		wantSize, wantSHA, found := pinned(name)
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

		got, err := sha256File(path)
		if err != nil {
			return fmt.Errorf("snapshot check: %w", err)
		}
		if got != wantSHA {
			return fmt.Errorf("%s has the documented size but sha256 %s, want %s — same length, "+
				"different bytes (pass -any-snapshot to measure something else deliberately)",
				path, got, wantSHA)
		}
	}
	return nil
}

// provenanceFile is where build records what it built from, inside the index directory
// so it travels with the thing it describes. The name stays clear of MANIFEST, which
// pkg/engine reserves; engine ignores any entry that is neither MANIFEST.tmp nor
// seg-prefixed, so this sits beside the segments without being debris to them.
const provenanceFile = "provenance.json"

// provenance is an index's own account of where it came from.
//
// The snapshot table pins the inputs a *command invocation* reads. It says nothing
// about the index, and the index is what the arms are measured over. So `build
// -any-snapshot` against another corpus revision, followed by `run` beside the pinned
// queries and qrels, passed every check there was: the two files run verifies are the
// pinned ones, and a revision that kept the same document keys leaves the qrels check
// finding every judgment present. Different document text, different vectors and
// different links then produce different rankings, which run, sweep and weights print
// under the labels docs/EVAL.md publishes.
//
// Recorded rather than inferred, because a committed index holds documents, not the
// name or the bytes of the file they were read out of.
type provenance struct {
	// Corpus is the sha256 of the corpus.jsonl the documents came from, hashed at
	// build time whether or not the snapshot check ran — an -any-snapshot build is
	// described exactly as faithfully as a pinned one, it simply does not match.
	Corpus string `json:"corpus_sha256"`

	// Partial records that the index was built with -partial, over a Semantic
	// Scholar cache not covering the corpus. build already prints that such arms must
	// not be published, and this is what lets a later command act on it rather than
	// trust that the operator read the log.
	Partial bool `json:"partial"`
}

// writeProvenance records what an index was built from. Written after the commit, so a
// file that exists describes an index that exists.
func writeProvenance(indexPath string, p provenance) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	path := filepath.Join(indexPath, provenanceFile)
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// verifyProvenance refuses an index that was not built from the pinned corpus, or that
// was built over a cache not covering it.
//
// A missing file is refused rather than tolerated. It means an index committed before
// this record existed, and the whole point is that such an index cannot say what it
// came from; accepting it would leave the gap open for exactly the indexes most likely
// to be stale.
func verifyProvenance(indexPath string) error {
	path := filepath.Join(indexPath, provenanceFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist, so this index cannot say which corpus it was built "+
			"from; rebuild with `weft-eval build` (pass -any-snapshot to measure an index whose "+
			"provenance is not checked)", path)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var p provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	_, want, ok := pinned(corpusFile)
	if !ok {
		return fmt.Errorf("verifyProvenance: %s is not in the pinned snapshot table", corpusFile)
	}
	if p.Corpus != want {
		return fmt.Errorf("%s says this index was built from a corpus.jsonl with sha256 %s, and the "+
			"release docs/EVAL.md section 5.1 publishes is %s; the queries and qrels beside it are the "+
			"pinned ones, so nothing else here would have noticed (pass -any-snapshot to measure this "+
			"index deliberately)", path, p.Corpus, want)
	}
	if p.Partial {
		return fmt.Errorf("%s says this index was built with -partial, over a Semantic Scholar cache "+
			"that does not cover the corpus, so its vector and graph arms measure a corpus that is "+
			"partly text-only (pass -any-snapshot to measure it deliberately)", path)
	}
	return nil
}
