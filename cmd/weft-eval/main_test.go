package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyoo2003/weft/internal/eval"
)

// writeCache writes s to a temporary s2.jsonl and returns its path.
func writeCache(t *testing.T, s string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s2.jsonl")
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	return path
}

// linesParse reports whether every non-empty line of the file is a complete JSON
// value on its own. This is the property Go's stream decoder does not check and the
// Python side depends on: testdata/gen_query_vectors.py --verify reads s2.jsonl with
// one json.loads per line.
func linesParse(t *testing.T, path string) error {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var v eval.S2Record
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return err
		}
	}
	return nil
}

// TestDoneKeysKeepsTheRecordSeparator is the regression for the resumable cache's
// worst failure mode. json.Decoder.InputOffset stops at the closing brace of the last
// value, one byte before the newline the encoder wrote after it; an offset that
// excluded that newline made prepare read a healthy file as carrying a one-byte
// fragment, truncate the separator away, and append the next record onto the same
// line.
func TestDoneKeysKeepsTheRecordSeparator(t *testing.T) {
	path := writeCache(t, `{"key":"a"}`+"\n"+`{"key":"b"}`+"\n")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	done, good, err := doneKeys(path)
	if err != nil {
		t.Fatalf("doneKeys: %v", err)
	}
	if len(done) != 2 || !done["a"] || !done["b"] {
		t.Errorf("done = %v, want a and b", done)
	}
	if good != fi.Size() {
		t.Fatalf("good offset %d, file is %d bytes: a complete file has nothing to "+
			"truncate, and dropping the difference removes the record separator", good, fi.Size())
	}
}

// TestDoneKeysDropsOnlyTheTruncatedRecord: an interrupted run leaves a half-written
// value after a valid separator. The fragment has to go and the separator has to
// stay, and the check is that appending afterwards still produces one record per
// line.
func TestDoneKeysDropsOnlyTheTruncatedRecord(t *testing.T) {
	path := writeCache(t, `{"key":"a"}`+"\n"+`{"key":"b"}`+"\n"+`{"key":"c","ref`)

	done, good, err := doneKeys(path)
	if err != nil {
		t.Fatalf("doneKeys: %v", err)
	}
	if len(done) != 2 || done["c"] {
		t.Errorf("done = %v, want only the two complete records", done)
	}
	if err := os.Truncate(path, good); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(eval.S2Record{Key: "c"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := linesParse(t, path); err != nil {
		t.Errorf("after resume the cache is no longer one record per line: %v", err)
	}
	again, _, err := doneKeys(path)
	if err != nil {
		t.Fatalf("doneKeys after resume: %v", err)
	}
	if len(again) != 3 || !again["c"] {
		t.Errorf("done = %v, want a, b and c", again)
	}
}

// TestCorpusIDIndexIsDeterministic is the reproducibility guard. CORD-19 ships the
// same paper under several cord_uids, so the CorpusId -> cord_uid mapping has to pick
// one; ranging over a Go map picked a different one per build, which silently made
// every published number a draw from a distribution rather than a measurement.
//
// Run repeatedly on purpose: map iteration order is randomised per range statement,
// so a single comparison would pass most of the time even with the bug present.
func TestCorpusIDIndexIsDeterministic(t *testing.T) {
	recs := map[string]eval.S2Record{
		"zzz": {Key: "zzz", CorpusID: "100"},
		"aaa": {Key: "aaa", CorpusID: "100"},
		"mmm": {Key: "mmm", CorpusID: "100"},
		"bbb": {Key: "bbb", CorpusID: "200"},
		"ccc": {Key: "ccc"}, // Unjoinable: a tombstone written by prepare.
	}

	want, wantDup := corpusIDIndex(recs)
	if wantDup != 2 {
		t.Errorf("duplicated = %d, want 2", wantDup)
	}
	if want["100"] != "aaa" {
		t.Errorf("CorpusId 100 -> %q, want the lowest cord_uid %q", want["100"], "aaa")
	}
	if want["200"] != "bbb" {
		t.Errorf("CorpusId 200 -> %q, want %q", want["200"], "bbb")
	}
	if _, ok := want[""]; ok {
		t.Error("the empty CorpusId became a key; a tombstone resolves nothing")
	}

	for i := range 200 {
		got, dup := corpusIDIndex(recs)
		if dup != wantDup || len(got) != len(want) {
			t.Fatalf("run %d: %d ids / %d duplicated, want %d / %d", i, len(got), dup, len(want), wantDup)
		}
		for id, key := range want {
			if got[id] != key {
				t.Fatalf("run %d: CorpusId %s -> %q, the first run said %q", i, id, got[id], key)
			}
		}
	}
}

// TestReadS2RecordsAcceptsTombstones: prepare records a key-only entry for a document
// with no Semantic Scholar side, so a resume stops asking about it. build has to read
// those back as "known, nothing attached" rather than reject them.
func TestReadS2RecordsAcceptsTombstones(t *testing.T) {
	path := writeCache(t, `{"key":"a"}`+"\n"+`{"key":"b","corpus_id":"7","refs":["9"]}`+"\n")

	recs, err := readS2Records(path)
	if err != nil {
		t.Fatalf("readS2Records: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if r := recs["a"]; r.CorpusID != "" || len(r.Refs) != 0 || len(r.Vector) != 0 {
		t.Errorf("tombstone read back as %+v, want an empty record", r)
	}
	if r := recs["b"]; r.CorpusID != "7" || len(r.Refs) != 1 {
		t.Errorf("record read back as %+v, want CorpusID 7 with one ref", r)
	}
}
