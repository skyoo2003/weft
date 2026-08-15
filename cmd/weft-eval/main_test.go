// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/skyoo2003/weft/internal/eval"
	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/fusion"
)

// candStream builds a candidate list in the given rank order. The scores descend so
// the list is well-formed, and they are otherwise meaningless: neither fusion.Fuse nor
// rrf reads Candidate.Score, which is the property TestScoresAreNeverRead pins one
// package over.
func candStream(ids ...engine.DocID) []engine.Candidate {
	out := make([]engine.Candidate, len(ids))
	for i, id := range ids {
		out[i] = engine.Candidate{Doc: id, Score: float64(len(ids) - i)}
	}
	return out
}

// TestRRFAtTheLibraryConstantIsFuse pins the copy in run.go to the library it mirrors.
//
// rrf exists because the sweep has to vary a constant fusion.RRFk fixes, and
// docs/EVAL.md section 2.1 cites that as what injecting the Fuser bought. What the
// argument does not buy is protection from drift: rrf reimplements the accumulation, so
// a change to fusion's — a new guard, a different summation order — would leave the
// sweep measuring a function the library no longer has, and every cell of the section
// 5.10 table would come from code nobody compared.
//
// Bit equality, not approximate. At RRFk the two are meant to be the same arithmetic in
// the same order, so a tolerance here would hide exactly the reordering the rank-major
// loop exists to prevent.
func TestRRFAtTheLibraryConstantIsFuse(t *testing.T) {
	cases := []struct {
		name    string
		streams [][]engine.Candidate
	}{
		{"streams of different depths", [][]engine.Candidate{
			candStream(1, 3, 7, 2),
			candStream(3, 9, 1),
			candStream(7, 4),
		}},
		// Doc 1 holds ranks 1, 2, 7 and doc 2 holds 7, 1, 2 — the same multiset in a
		// different order. Rank-major accumulation sums them to identical bits;
		// stream-major would not, so a copy that swept streams first passes every case
		// above and fails this one.
		{"equal rank multisets", [][]engine.Candidate{
			candStream(1, 90, 91, 92, 93, 94, 2),
			candStream(2, 1),
			candStream(95, 2, 96, 97, 98, 99, 1),
		}},
		{"one stream is empty", [][]engine.Candidate{candStream(1, 2), {}}},
		{"no streams at all", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []int{1, 3, 10} {
				got, want := rrf(fusion.RRFk)(tc.streams, k), fusion.Fuse(tc.streams, k)
				if !slices.Equal(got, want) {
					t.Errorf("k=%d: rrf(fusion.RRFk) gave %+v, fusion.Fuse gave %+v", k, got, want)
				}
			}
		})
	}

	// And the rank constant is live. Without this the cases above are satisfied by an
	// rrf that ignores its parameter — which would print 28 identical cells and let
	// sweep report "no sign flip" from one configuration measured 28 times.
	streams := [][]engine.Candidate{candStream(1, 2, 3), candStream(3, 2, 1)}
	if slices.Equal(rrf(1)(streams, 3), fusion.Fuse(streams, 3)) {
		t.Error("rrf(1) fused identically to fusion.Fuse at RRFk=60; the rank constant is not reaching the sum")
	}
}

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

// TestScanS2CacheKeepsTheRecordSeparator is the regression for the resumable cache's
// worst failure mode. json.Decoder.InputOffset stops at the closing brace of the last
// value, one byte before the newline the encoder wrote after it; an offset that
// excluded that newline made prepare read a healthy file as carrying a one-byte
// fragment, truncate the separator away, and append the next record onto the same
// line.
func TestScanS2CacheKeepsTheRecordSeparator(t *testing.T) {
	path := writeCache(t, `{"key":"a"}`+"\n"+`{"key":"b"}`+"\n")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	cache, err := scanS2Cache(path, nil)
	if err != nil {
		t.Fatalf("scanS2Cache: %v", err)
	}
	if len(cache.keys) != 2 || !cache.keys["a"] || !cache.keys["b"] {
		t.Errorf("keys = %v, want a and b", cache.keys)
	}
	if good := cache.good; good != fi.Size() {
		t.Fatalf("good offset %d, file is %d bytes: a complete file has nothing to "+
			"truncate, and dropping the difference removes the record separator", good, fi.Size())
	}
}

// TestScanS2CacheDropsOnlyTheTruncatedRecord: an interrupted run leaves a half-written
// value after a valid separator. The fragment has to go and the separator has to
// stay, and the check is that appending afterwards still produces one record per
// line.
func TestScanS2CacheDropsOnlyTheTruncatedRecord(t *testing.T) {
	path := writeCache(t, `{"key":"a"}`+"\n"+`{"key":"b"}`+"\n"+`{"key":"c","ref`)

	cache, err := scanS2Cache(path, nil)
	if err != nil {
		t.Fatalf("scanS2Cache: %v", err)
	}
	if len(cache.keys) != 2 || cache.keys["c"] {
		t.Errorf("keys = %v, want only the two complete records", cache.keys)
	}
	if err := os.Truncate(path, cache.good); err != nil {
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
	again, err := scanS2Cache(path, nil)
	if err != nil {
		t.Fatalf("scanS2Cache after resume: %v", err)
	}
	if len(again.keys) != 3 || !again.keys["c"] {
		t.Errorf("keys = %v, want a, b and c", again.keys)
	}
}

// TestScanS2CacheRefusesDamageInTheMiddle is the other half of the previous test, and
// the reason the two cases have to be told apart at all.
//
// json.Decoder reports "half-written tail" and "damaged record with valid ones after
// it" as the same error, and prepare's response to a short offset is os.Truncate. Read
// as a tail, mid-file damage therefore deletes every record following it — hours of
// rate-limited fetching — logs it as an incomplete trailing record, and the next run
// refetches what it no longer has. Nothing in that sequence looks like a failure.
//
// The remaining valid records are what makes the difference visible: a record that
// never finished writing is the last line of the file, so a newline after the damage
// means a line that did finish.
func TestScanS2CacheRefusesDamageInTheMiddle(t *testing.T) {
	// Two good records, then a broken one, then more good ones that truncation would
	// take with it.
	path := writeCache(t, `{"key":"a"}`+"\n"+`{"key":"b"}`+"\n"+`{"key":"c`+"\n"+
		`{"key":"d"}`+"\n"+`{"key":"e"}`+"\n")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	cache, err := scanS2Cache(path, nil)
	if err == nil {
		t.Fatalf("scanS2Cache accepted a damaged cache and offered to truncate %d of %d bytes, "+
			"which drops keys d and e", before.Size()-cache.good, before.Size())
	}
	for _, want := range []string{"middle", "refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q; the operator has to be told this is not a "+
				"resumable interruption", err, want)
		}
	}

	// The caller gets the zero value on error, so a caller that ignores the error
	// truncates to 0 rather than to a plausible-looking offset.
	if cache.good != 0 || len(cache.keys) != 0 {
		t.Errorf("cache = %+v, want the zero value on error", cache)
	}

	// A complete but unparseable final line is damage too, not an interrupted write:
	// an interrupted write cannot have produced the newline that terminates it.
	tail := writeCache(t, `{"key":"a"}`+"\n"+`{"key":"b`+"\n")
	if _, err := scanS2Cache(tail, nil); err == nil {
		t.Error("scanS2Cache accepted a terminated line that does not parse")
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

	corpus := map[string]bool{"zzz": true, "aaa": true, "mmm": true, "bbb": true, "ccc": true}

	want, wantDup, foreign := corpusIDIndex(recs, corpus)
	if foreign != 0 {
		t.Errorf("foreign = %d, want 0: every record here is in the corpus", foreign)
	}
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
		got, dup, _ := corpusIDIndex(recs, corpus)
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

// TestCorpusIDIndexIgnoresRecordsOutsideTheCorpus: a cache from a larger or older
// corpus can cover every current key and still hold records for documents that are
// gone. Those keys used to take part in the same sorted first-writer-wins race, so
// one of them sorting below the present copy handed the CorpusId mapping to a key
// the index does not hold — every citation to that paper then resolved to nothing
// and real in-corpus edges were dropped as dangling, with the coverage gate
// satisfied because the cache did cover the corpus.
func TestCorpusIDIndexIgnoresRecordsOutsideTheCorpus(t *testing.T) {
	recs := map[string]eval.S2Record{
		// Sorts below "m" and shares its CorpusId: the stale record wins the race
		// unless corpus membership is checked first.
		"aaa": {Key: "aaa", CorpusID: "100"},
		"m":   {Key: "m", CorpusID: "100"},
		"n":   {Key: "n", CorpusID: "200", Refs: []string{"100"}},
	}
	corpus := map[string]bool{"m": true, "n": true}

	got, dup, foreign := corpusIDIndex(recs, corpus)
	if foreign != 1 {
		t.Errorf("foreign = %d, want 1", foreign)
	}
	if got["100"] != "m" {
		t.Errorf("CorpusId 100 -> %q, want the in-corpus key %q: n's citation resolves to a "+
			"document this index does not hold and the edge is silently lost", got["100"], "m")
	}
	// The stale record is skipped before the duplicate check, so it must not be
	// reported as a cord_uid that merely lost a race with a sibling copy.
	if dup != 0 {
		t.Errorf("duplicated = %d, want 0: a record outside the corpus is not a duplicate", dup)
	}
}

// TestPrepareRefusesAJoinWithNoEvidenceItCanWork is the guard on the one error that
// means two different things. ReadCORD19IDs reports ErrEmptyDataset both when every
// joinable document is already cached — the normal end of a resumed run — and when
// the metadata release does not match this corpus at all. Treated as the first, the
// second writes an asked-and-unjoinable record for every document, build's coverage
// gate then reads the cache as complete, and the vector and graph arms get published
// over an index with no vectors and no edges.
func TestPrepareRefusesAJoinWithNoEvidenceItCanWork(t *testing.T) {
	const metadata = "cord_uid,doi,pmcid,pubmed_id,s2_id\nx,,,,999\n" // no row for a or b
	dir := evalDir(t, map[string]string{
		corpusFile:   twoDocCorpus,
		metadataFile: metadata,
	})

	err := prepare(context.Background(), []string{"-data", dir, "-any-snapshot"})
	if err == nil {
		t.Fatal("prepare succeeded against metadata matching no document in the corpus; it " +
			"would tombstone the whole corpus and let build publish empty vector and graph arms")
	}
	if _, statErr := os.Stat(filepath.Join(dir, s2File)); statErr == nil {
		t.Error("a cache was written for a join that never worked")
	}

	// The same zero-match answer is the normal end of a resumed run, and the cache is
	// what tells them apart: a record carrying a CorpusId is evidence the join worked
	// before, so the remaining keys really are the unjoinable ones.
	dir = evalDir(t, map[string]string{
		corpusFile:   twoDocCorpus,
		metadataFile: metadata,
		s2File:       `{"key":"a","corpus_id":"100"}` + "\n",
	})
	if err := prepare(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Fatalf("prepare with prior evidence of a working join: %v", err)
	}
	cache, err := scanS2Cache(filepath.Join(dir, s2File), nil)
	if err != nil {
		t.Fatalf("scanS2Cache: %v", err)
	}
	if !cache.keys["b"] {
		t.Error("b was not recorded as asked-and-unjoinable, so every rerun rescans the metadata for it")
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

// TestReadS2RecordsRejectsADuplicateKey: prepare writes one record per document, so a
// second record for a key means two caches were concatenated and the later one wins on
// file position alone. The damaging order is this one — a full record followed by a
// tombstone — because it drops a vector and a reference list while the coverage gate
// still counts the document as cached.
func TestReadS2RecordsRejectsADuplicateKey(t *testing.T) {
	path := writeCache(t, `{"key":"a","corpus_id":"7","refs":["9"],"vec":[1,2]}`+"\n"+`{"key":"a"}`+"\n")

	recs, err := readS2Records(path)
	if err == nil {
		t.Fatalf("readS2Records accepted a duplicate key; a is %+v, silently the later record", recs["a"])
	}
	if !errors.Is(err, eval.ErrBadRecord) {
		t.Errorf("error = %v, want ErrBadRecord", err)
	}
}

// TestScanS2CacheTalliesTheModelsAlreadyCached is the resume regression for embedding
// provenance. The model tally used to live only in the invocation that fetched a
// batch, so a prepare that died mid-run took the provenance of everything it had
// written with it, and the run that finished the job reported only its own tail as
// clean. Two embedding spaces in one cache would then reach build with nothing said.
func TestScanS2CacheTalliesTheModelsAlreadyCached(t *testing.T) {
	path := writeCache(t, strings.Join([]string{
		`{"key":"a","vec":[1,2],"model":"specter_v2"}`,
		`{"key":"b","vec":[1,2],"model":"specter_v1"}`,
		`{"key":"c","vec":[1,2]}`, // written before the field existed
		`{"key":"d"}`,             // tombstone: no vector, so no embedding space
		"",
	}, "\n"))

	cache, err := scanS2Cache(path, nil)
	if err != nil {
		t.Fatalf("scanS2Cache: %v", err)
	}
	models := cache.models
	want := map[string]int{"specter_v2": 1, "specter_v1": 1, "": 1}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	for m, n := range want {
		if models[m] != n {
			t.Errorf("models[%q] = %d, want %d", m, models[m], n)
		}
	}
}

// TestS2RecordRoundTripsTheModel: the tally above is only as good as the field
// surviving a write and a read, and omitempty means an absent model and an empty one
// are the same bytes — which is the intended reading, "provenance not recorded".
func TestS2RecordRoundTripsTheModel(t *testing.T) {
	b, err := json.Marshal(eval.S2Record{Key: "a", Vector: []float32{1}, Model: eval.S2Model})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got eval.S2Record
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if got.Model != eval.S2Model {
		t.Errorf("Model = %q, want %q (wire form %s)", got.Model, eval.S2Model, b)
	}
	b, err = json.Marshal(eval.S2Record{Key: "a"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "model") {
		t.Errorf("tombstone marshalled as %s, want no model field", b)
	}
}

// TestVerifySnapshotCatchesTruncationAndSameLengthEdits covers both halves of the
// pinned check, because each catches a different accident. Size catches the one that
// actually happens — an interrupted download of a 1.6 GB file, or a different release —
// and catches it from a stat rather than by hashing gigabytes. The hash catches what
// size cannot see: a file edited in place, where a published number would come from
// bytes nobody downloaded.
//
// The expected size is read out of the table rather than written here, so this test
// cannot drift from the pin it is checking.
func TestVerifySnapshotCatchesTruncationAndSameLengthEdits(t *testing.T) {
	var want int64
	for _, s := range snapshot {
		if s.name == queriesFile {
			want = s.size
		}
	}
	if want == 0 {
		t.Fatalf("%s is not in the snapshot table", queriesFile)
	}

	dir := evalDir(t, map[string]string{queriesFile: "{}\n"})
	err := verifySnapshot(dir, queriesFile)
	if err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("truncated file: error = %v, want it to report the size", err)
	}

	// Same length, different content: only the hash separates these.
	if err := os.WriteFile(filepath.Join(dir, queriesFile),
		bytes.Repeat([]byte("x"), int(want)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	err = verifySnapshot(dir, queriesFile)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("same-size edit: error = %v, want it to report the hash", err)
	}

	// A name that is not pinned is a mistake in this command, not a bad input, and must
	// not read as a passing check.
	if err := verifySnapshot(dir, s2File); err == nil {
		t.Error("an unpinned name verified successfully")
	}
}

// evalDir lays out a minimal -data directory: one file per entry, parents created.
func evalDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

const twoDocCorpus = `{"_id":"a","title":"one","text":"alpha"}
{"_id":"b","title":"two","text":"beta"}
`

// TestBuildRefusesAnIncompleteCache is the guard against the failure documented in
// docs/EVAL.md section 4.1: an index built over a corpus the Semantic Scholar cache
// only partly covers still runs, still reports arms named text+vector and
// text+vector+graph, and produces numbers that read as a measurement.
//
// prepare writes a record for every key it asks about — a bare tombstone when the
// document has no Semantic Scholar side at all — so a finished prepare covers the
// corpus exactly and a missing key can only mean the job did not finish.
func TestBuildRefusesAnIncompleteCache(t *testing.T) {
	dir := evalDir(t, map[string]string{
		corpusFile: twoDocCorpus,
		s2File:     `{"key":"a"}` + "\n", // b was never asked about
	})

	err := build(context.Background(), []string{"-data", dir, "-any-snapshot"})
	if err == nil {
		t.Fatal("build succeeded on a cache covering 1 of 2 documents; it must refuse, " +
			"or half the corpus enters the index text-only under a text+vector+graph label")
	}
	if !strings.Contains(err.Error(), "-partial") {
		t.Errorf("error = %v, want it to name the -partial opt-in", err)
	}

	// The same build is allowed when it is asked for, because smoke-testing the
	// pipeline on a slice is a real use — it just must not be the default.
	if err := build(context.Background(), []string{"-data", dir, "-partial", "-any-snapshot"}); err != nil {
		t.Fatalf("build -partial: %v", err)
	}
}

// TestBuildAcceptsATombstoneOnlyCache: a document prepare asked about and found
// nothing for is covered, not missing. Confusing the two would make the check fire
// on every honest full build of TREC-COVID, where 8,495 documents have no usable
// identifier at all.
func TestBuildAcceptsATombstoneOnlyCache(t *testing.T) {
	dir := evalDir(t, map[string]string{
		corpusFile: twoDocCorpus,
		s2File:     `{"key":"a"}` + "\n" + `{"key":"b"}` + "\n",
	})
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Fatalf("build over a complete tombstone-only cache: %v", err)
	}
}

// TestDominantDimIgnoresAnOutlierAndForeignRecords covers the two rules that decide
// which width a build indexes at, before any document is tokenised.
func TestDominantDimIgnoresAnOutlierAndForeignRecords(t *testing.T) {
	vec := func(n int) eval.S2Record {
		return eval.S2Record{Vector: make([]float32, n)}
	}
	tests := []struct {
		name   string
		recs   map[string]eval.S2Record
		corpus []string
		want   int
	}{
		{"one outlier does not win", map[string]eval.S2Record{
			"a": vec(2), "b": vec(768), "c": vec(768),
		}, []string{"a", "b", "c"}, 768},
		{"a record outside the corpus does not vote", map[string]eval.S2Record{
			"gone": vec(2), "a": vec(768),
		}, []string{"a"}, 768},
		{"a record with no vector does not vote", map[string]eval.S2Record{
			"a": {}, "b": vec(768),
		}, []string{"a", "b"}, 768},
		// Stable rather than clever: a tie means two embedding spaces in equal
		// measure, and what matters is that two builds over one cache agree.
		{"a tie takes the narrower width", map[string]eval.S2Record{
			"a": vec(2), "b": vec(768),
		}, []string{"a", "b"}, 2},
		{"no vectors at all", map[string]eval.S2Record{"a": {}}, []string{"a"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			corpus := make(map[string]bool, len(tc.corpus))
			for _, k := range tc.corpus {
				corpus[k] = true
			}
			// Run it a few times: recs is a map, so a rule that depended on
			// iteration order would pass intermittently rather than fail.
			for range 8 {
				if got, _ := dominantDim(tc.recs, corpus); got != tc.want {
					t.Fatalf("dominantDim = %d, want %d", got, tc.want)
				}
			}
		})
	}
}

// TestBuildIndexesTheDominantVectorWidth is the same rule end to end, in the shape that
// makes it matter.
//
// The width used to be whichever vector the corpus order presented first. One malformed
// response ahead of an otherwise uniform corpus therefore became authoritative, every
// correct vector behind it was skipped for width — and build still committed, so the run
// went on to publish a vector arm backed by the single outlier.
func TestBuildIndexesTheDominantVectorWidth(t *testing.T) {
	dir := evalDir(t, map[string]string{
		// "bad" is first in corpus order, which is the whole point.
		corpusFile: `{"_id":"bad","title":"one","text":"alpha"}` + "\n" +
			`{"_id":"x","title":"two","text":"beta"}` + "\n" +
			`{"_id":"y","title":"three","text":"gamma"}` + "\n",
		s2File: `{"key":"bad","vec":[1]}` + "\n" +
			`{"key":"x","vec":[1,0,0]}` + "\n" +
			`{"key":"y","vec":[0,1,0]}` + "\n",
	})
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Fatalf("build: %v", err)
	}

	ix, err := engine.Open(filepath.Join(dir, indexDir))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for key, want := range map[string]int{"x": 3, "y": 3, "bad": 0} {
		id, ok := ix.Resolve(key)
		if !ok {
			t.Fatalf("%s is not in the index", key)
		}
		doc, ok := ix.Doc(id)
		if !ok {
			t.Fatalf("Doc(%s): not found", key)
		}
		if len(doc.Vector) != want {
			t.Errorf("%s has a %d-dim vector, want %d: the majority width is what gets "+
				"indexed, not the first one seen", key, len(doc.Vector), want)
		}
	}
}

// TestBuildCountsAnEdgeOnce pins what "579,719 in-corpus edges" is allowed to mean.
//
// A references list can name the same CorpusId twice, and duplicate-paper merging on
// the Semantic Scholar side can resolve one back to the citing document itself.
// Appended blindly, both become entries in Document.Links and both increment the edge
// total build prints and docs/EVAL.md publishes — while the traversal, which dedupes on
// visit and gets nowhere from a self-edge, sees neither. That is a density claim no
// ranking can corroborate or contradict, which is the one kind this harness exists to
// keep out.
func TestBuildCountsAnEdgeOnce(t *testing.T) {
	dir := evalDir(t, map[string]string{
		corpusFile: `{"_id":"a","title":"one","text":"alpha"}` + "\n" +
			`{"_id":"b","title":"two","text":"beta"}` + "\n",
		s2File: `{"key":"a","corpus_id":"1","refs":["2","2","1"]}` + "\n" +
			`{"key":"b","corpus_id":"2"}` + "\n",
	})
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Fatalf("build: %v", err)
	}

	ix, err := engine.Open(filepath.Join(dir, indexDir))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id, ok := ix.Resolve("a")
	if !ok {
		t.Fatal("a is not in the index")
	}
	doc, ok := ix.Doc(id)
	if !ok {
		t.Fatal("Doc(a): not found")
	}
	if !slices.Equal(doc.Links, []string{"b"}) {
		t.Errorf("a links to %v, want [b]: three references — b twice and a itself — are "+
			"one edge, and counting them as three inflates a published graph density", doc.Links)
	}
}

// TestLoadQueriesRejectsVectorsFromAnOlderQuerySet is the pairing regression. Query
// ids are short and stable — trec-covid numbers them "1" to "50" — so a vector file
// generated before the query text was last edited matches every id, covers every
// query, and satisfies the all-or-none check while carrying embeddings of different
// questions. The text is what distinguishes them, and it is only worth the
// generator writing it if something reads it.
func TestLoadQueriesRejectsVectorsFromAnOlderQuerySet(t *testing.T) {
	files := map[string]string{
		queriesFile:  `{"_id":"1","text":"what is the origin of COVID-19"}` + "\n",
		qrelsFile:    "query-id\tcorpus-id\tscore\n1\ta\t2\n",
		queryVecFile: `{"id":"1","text":"a question from an older snapshot","vec":[0.1,0.2]}` + "\n",
	}
	if _, err := loadQueries(evalDir(t, files)); err == nil {
		t.Fatal("loadQueries accepted a vector embedded from different text; the arm would " +
			"be published as text+vector over the wrong questions")
	}

	files[queryVecFile] = `{"id":"1","text":"what is the origin of COVID-19","vec":[0.1,0.2]}` + "\n"
	qs, err := loadQueries(evalDir(t, files))
	if err != nil {
		t.Fatalf("loadQueries with matching text: %v", err)
	}
	if len(qs) != 1 || len(qs[0].Query.Vector) != 2 {
		t.Fatalf("loaded %+v, want one query carrying its vector", qs)
	}
}

// TestPrepareRefusesJoinEvidenceAboutAnotherCorpus is the guard above reached by the
// route that used to walk past it.
//
// The evidence that a join can work is a cached record carrying a CorpusId. Counted
// over the whole file, an s2.jsonl kept from an earlier dataset supplies it with keys
// this corpus has never heard of — so zero of the current keys match the metadata, the
// guard sees what looks like a working join, and every current document is written as
// a tombstone. build then reads complete coverage and publishes vector and graph arms
// over an index holding neither, which is the failure the guard exists to prevent,
// rebuilt out of the guard.
func TestPrepareRefusesJoinEvidenceAboutAnotherCorpus(t *testing.T) {
	const metadata = "cord_uid,doi,pmcid,pubmed_id,s2_id\nx,,,,999\n" // no row for a or b
	dir := evalDir(t, map[string]string{
		corpusFile:   twoDocCorpus,
		metadataFile: metadata,
		// z is not in this corpus. Under the old unscoped count it was still evidence.
		s2File: `{"key":"z","corpus_id":"100"}` + "\n",
	})

	if err := prepare(context.Background(), []string{"-data", dir, "-any-snapshot"}); err == nil {
		t.Fatal("prepare accepted a join whose only evidence is a record about another corpus; " +
			"it would tombstone a and b and let build publish empty vector and graph arms")
	}

	// And the counter itself, so the failure above cannot be satisfied by some other
	// check happening to fire first.
	cache, err := scanS2Cache(filepath.Join(dir, s2File), map[string]bool{"a": true, "b": true})
	if err != nil {
		t.Fatalf("scanS2Cache: %v", err)
	}
	if cache.joined != 0 {
		t.Errorf("joined = %d, want 0: z carries a CorpusId but is not a document of this corpus",
			cache.joined)
	}
	if !cache.keys["z"] {
		t.Error("z was dropped from keys; scoping applies to the join tally, not to what the file holds")
	}
}

// TestBuildRecordsWhatItBuiltFrom pins the index's own account of its provenance.
//
// run, sweep and weights verify queries.jsonl and qrels/test.tsv — the files they read
// — and the index is a third input nothing checked. An index built from another corpus
// revision that kept the same document keys satisfies the qrels check too, so its
// different text, vectors and links rank differently under the labels docs/EVAL.md
// publishes.
func TestBuildRecordsWhatItBuiltFrom(t *testing.T) {
	dir := evalDir(t, map[string]string{
		corpusFile: twoDocCorpus,
		s2File:     `{"key":"a","corpus_id":"1"}` + "\n" + `{"key":"b","corpus_id":"2"}` + "\n",
	})
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Fatalf("build: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, indexDir, provenanceFile))
	if err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	var p provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("parse provenance: %v", err)
	}
	want, err := sha256File(filepath.Join(dir, corpusFile))
	if err != nil {
		t.Fatalf("hash corpus: %v", err)
	}
	if p.Corpus != want {
		t.Errorf("provenance records corpus %q, the corpus it read hashes to %q", p.Corpus, want)
	}
	if p.Partial {
		t.Error("a full build recorded itself as partial")
	}

	// The index still opens. provenance.json sits in the directory pkg/engine owns,
	// and engine refuses to commit over entries it did not write — so a name it does
	// not ignore would break the next build rather than this read.
	if _, err := engine.Open(filepath.Join(dir, indexDir)); err != nil {
		t.Errorf("open after writing provenance: %v", err)
	}
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Errorf("rebuild over an index carrying provenance: %v", err)
	}
}

// TestBuildCarriesUnpinnedPreparationIntoTheIndex closes the laundering route.
//
// `prepare -any-snapshot` says the join ran against inputs docs/EVAL.md does not pin,
// and until now that claim died with the command. The cache it leaves is an ordinary
// complete one, so a later plain `build` finds full coverage, hashes a corpus that does
// match, and records partial=false — an index every check downstream accepts, whose
// citation edges and SPECTER vectors came from a metadata release nothing verified.
// The second build is the one that matters here: it passes no flag at all.
func TestBuildCarriesUnpinnedPreparationIntoTheIndex(t *testing.T) {
	dir := evalDir(t, map[string]string{
		corpusFile: twoDocCorpus,
		s2File:     `{"key":"a","corpus_id":"1"}` + "\n" + `{"key":"b","corpus_id":"2"}` + "\n",
	})
	if err := os.WriteFile(filepath.Join(dir, s2UnpinnedFile), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	// -any-snapshot here is only about this fixture's corpus.jsonl not being the
	// pinned 221 MB one; the flag under test is the one prepare left behind.
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Fatalf("build: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, indexDir, provenanceFile))
	if err != nil {
		t.Fatalf("read provenance: %v", err)
	}
	var p provenance
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("parse provenance: %v", err)
	}
	if !p.PrepareUnpinned {
		t.Error("the index does not record that its cache was joined against unpinned inputs, " +
			"so a later run would publish its vector and graph arms")
	}

	// And the marker survives a build, because the cache it describes does.
	if _, err := os.Stat(filepath.Join(dir, s2UnpinnedFile)); err != nil {
		t.Errorf("build removed the marker beside the cache: %v", err)
	}
}

// TestPrepareMarksAnUnpinnedJoin is the other half: the marker has to exist before
// build can carry it, and prepare is what writes it.
func TestPrepareMarksAnUnpinnedJoin(t *testing.T) {
	dir := evalDir(t, map[string]string{
		corpusFile:   twoDocCorpus,
		metadataFile: "cord_uid,doi,pmcid,pubmed_id,s2_id\na,,,,1\nb,,,,2\n",
		// Both documents already cached, so prepare finishes without an API call.
		s2File: `{"key":"a","corpus_id":"1"}` + "\n" + `{"key":"b","corpus_id":"2"}` + "\n",
	})

	if err := prepare(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, s2UnpinnedFile)); err != nil {
		t.Fatalf("prepare -any-snapshot left no marker beside the cache: %v", err)
	}
}

// TestVerifyProvenanceRefusesAnIndexItCannotPublish covers the answers that are not
// "this is the published index": a corpus that is not the pinned one, a build that did
// not cover the corpus, an index too old to say either way, and a cache whose join ran
// against inputs the snapshot table does not pin.
func TestVerifyProvenanceRefusesAnIndexItCannotPublish(t *testing.T) {
	_, wantSHA, ok := pinned(corpusFile)
	if !ok {
		t.Fatal("corpus.jsonl is not in the pinned snapshot table")
	}

	write := func(t *testing.T, p provenance) string {
		t.Helper()
		dir := t.TempDir()
		if err := writeProvenance(dir, p); err != nil {
			t.Fatalf("writeProvenance: %v", err)
		}
		return dir
	}

	t.Run("a corpus that is not the pinned one", func(t *testing.T) {
		dir := write(t, provenance{Corpus: "0000000000000000000000000000000000000000000000000000000000000000"})
		if err := verifyProvenance(dir); err == nil {
			t.Error("accepted an index built from another corpus; the queries and qrels beside " +
				"it are the pinned ones, so nothing else would have noticed")
		}
	})
	t.Run("a partial build", func(t *testing.T) {
		dir := write(t, provenance{Corpus: wantSHA, Partial: true})
		if err := verifyProvenance(dir); err == nil {
			t.Error("accepted an index whose vector and graph arms measure a partly text-only corpus")
		}
	})
	t.Run("an index that cannot say", func(t *testing.T) {
		if err := verifyProvenance(t.TempDir()); err == nil {
			t.Error("accepted an index with no provenance at all, which is the one most likely stale")
		}
	})
	t.Run("a cache joined against unpinned inputs", func(t *testing.T) {
		dir := write(t, provenance{Corpus: wantSHA, PrepareUnpinned: true})
		if err := verifyProvenance(dir); err == nil {
			t.Error("accepted an index whose edges and vectors came from a metadata release " +
				"nothing verified; the corpus hash matches, so nothing else would have noticed")
		}
	})
	t.Run("the published index", func(t *testing.T) {
		dir := write(t, provenance{Corpus: wantSHA})
		if err := verifyProvenance(dir); err != nil {
			t.Errorf("refused the pinned corpus: %v", err)
		}
	})
}

// TestDeltaSignReadsTheDeltaAlone pins rule 2 to what section 4 wrote down. The sign
// used to come from the interval, which made a cell whose delta had flipped invisible
// to the flip count whenever its interval happened to span zero.
func TestDeltaSignReadsTheDeltaAlone(t *testing.T) {
	// The magnitudes are tiny on purpose: at this size the paired interval over 50
	// queries spans zero every time, and the sign is still the sign.
	for _, tc := range []struct {
		d    float64
		want int
	}{
		{+1e-9, 1}, {-1e-9, -1}, {0, 0}, {+0.5, 1}, {-0.5, -1},
	} {
		if got := deltaSign(tc.d); got != tc.want {
			t.Errorf("deltaSign(%v) = %d, want %d", tc.d, got, tc.want)
		}
	}
}

// TestRunRejectsTheResampleCountBeforeItMeasuresAnything: eval.BootstrapCI refuses
// iters <= 0, but it is reached after every arm has been evaluated — on the documented
// corpus, 50 queries against 171,332 documents with a brute-force vector scan each,
// spent to arrive at a flag error decidable before the index was opened.
//
// The empty directory is the assertion. If the check moved back below the index open,
// this would fail on a missing file instead.
func TestRunRejectsTheResampleCountBeforeItMeasuresAnything(t *testing.T) {
	for _, cmd := range []struct {
		name string
		fn   func(context.Context, []string) error
	}{
		{"run", run}, {"sweep", sweep}, {"weights", weights},
	} {
		t.Run(cmd.name, func(t *testing.T) {
			err := cmd.fn(context.Background(), []string{"-data", t.TempDir(), "-any-snapshot", "-iters=0"})
			if !errors.Is(err, eval.ErrNoIters) {
				t.Errorf("error = %v, want ErrNoIters: the count is decidable before any arm runs", err)
			}
		})
	}
}

// TestPrepareRejectsANegativeLimit: -limit is the smoke-test flag, so the value a typo
// produces is the one that matters. 0 means unlimited, and `limit > 0` read -1 as
// unlimited too — a mistyped `-limit=-1` started fetching the whole corpus, hours of
// rate-limited requests appending to the resumable cache, with nothing in the log
// saying the flag had been ignored.
func TestPrepareRejectsANegativeLimit(t *testing.T) {
	dir := evalDir(t, map[string]string{
		corpusFile:   twoDocCorpus,
		metadataFile: "cord_uid,doi,pmcid,pubmed_id,s2_id\na,,,,1\nb,,,,2\n",
	})

	err := prepare(context.Background(), []string{"-data", dir, "-any-snapshot", "-limit=-1"})
	if err == nil {
		t.Fatal("prepare accepted -limit=-1 and fell through to the unlimited path")
	}
	if !strings.Contains(err.Error(), "-limit") {
		t.Errorf("error = %v, want it to name the flag", err)
	}
	// Rejected before anything is written, so the typo costs a message rather than a
	// mutated cache.
	if _, statErr := os.Stat(filepath.Join(dir, s2File)); statErr == nil {
		t.Error("a cache was written for an invocation that was refused")
	}
}

// TestBuildRefusesVectorsFromAnotherEmbeddingModel: width is not provenance. SPECTER
// v1 and v2 both emit 768 dimensions, so dominantDim accepts either and engine.Add
// stores either, while the query vectors come from gen_query_vectors.py under
// eval.S2Model. Cosine similarity across two embedding spaces is not a similarity, and
// it arrives as a plausible vector baseline with a graph delta measured against it —
// with nothing downstream able to notice, because a committed index carries no model
// label and prepare's warning belongs to whichever invocation fetched the batch.
func TestBuildRefusesVectorsFromAnotherEmbeddingModel(t *testing.T) {
	files := map[string]string{
		corpusFile: twoDocCorpus,
		s2File: `{"key":"a","corpus_id":"1","vec":[1,2],"model":"specter_v1"}` + "\n" +
			`{"key":"b","corpus_id":"2","vec":[3,4],"model":"specter_v2"}` + "\n",
	}

	dir := evalDir(t, files)
	err := build(context.Background(), []string{"-data", dir, "-any-snapshot"})
	if err == nil {
		t.Fatal("build indexed a vector from another embedding model; the vector arm it " +
			"publishes is cosine similarity between two different spaces")
	}
	if !strings.Contains(err.Error(), "specter_v1") {
		t.Errorf("error = %v, want it to name the model that does not belong", err)
	}

	// -partial is the existing way to say "these arms are not publishable", and it is
	// the override here too — run, sweep and weights refuse such an index by its
	// provenance, so the escape hatch cannot become a published number.
	dir = evalDir(t, files)
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot", "-partial"}); err != nil {
		t.Fatalf("build -partial: %v", err)
	}
	if err := verifyProvenance(filepath.Join(dir, indexDir)); err == nil {
		t.Error("a -partial index passed the provenance check, so its arms can be published after all")
	}

	// An unrecorded model is warned about, not refused: it is what the committed
	// measurement was fetched into, all 148,232 of its vectors, so refusing it would
	// refuse the published index rather than a mistake.
	dir = evalDir(t, map[string]string{
		corpusFile: twoDocCorpus,
		s2File: `{"key":"a","corpus_id":"1","vec":[1,2]}` + "\n" +
			`{"key":"b","corpus_id":"2","vec":[3,4]}` + "\n",
	})
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Errorf("build refused a cache written before the model field existed: %v", err)
	}
}

// TestLoadQueriesRejectsVectorsFromAnotherEmbedding is the query side of the hazard
// checkVectorModels closes on the document side.
//
// A file generated from another adapter, base revision or local model configuration
// carries the right query id and the right question text, so every pairing check
// passes while the vector scorer computes cosine similarity between two embedding
// spaces. Absence is tolerated — the committed query-vectors.jsonl predates the field
// — and a recorded model that disagrees is not.
func TestLoadQueriesRejectsVectorsFromAnotherEmbedding(t *testing.T) {
	const q = `{"_id":"1","text":"what is the origin of COVID-19"}` + "\n"
	files := map[string]string{
		queriesFile: q,
		qrelsFile:   "query-id\tcorpus-id\tscore\n1\ta\t2\n",
		queryVecFile: `{"id":"1","text":"what is the origin of COVID-19","vec":[0.1,0.2],` +
			`"model":"allenai/specter2_base+some_other_adapter"}` + "\n",
	}
	if _, err := loadQueries(evalDir(t, files)); err == nil {
		t.Fatal("loadQueries accepted a query vector from another embedding; the vector arm " +
			"would be cosine similarity across two spaces, published under its usual name")
	}

	files[queryVecFile] = `{"id":"1","text":"what is the origin of COVID-19","vec":[0.1,0.2],` +
		`"model":"` + queryVecModel + `"}` + "\n"
	if _, err := loadQueries(evalDir(t, files)); err != nil {
		t.Errorf("loadQueries refused the model gen_query_vectors.py writes: %v", err)
	}

	// No model at all is what the committed file holds, and refusing it would refuse
	// the published measurement rather than a mistake.
	files[queryVecFile] = `{"id":"1","text":"what is the origin of COVID-19","vec":[0.1,0.2]}` + "\n"
	if _, err := loadQueries(evalDir(t, files)); err != nil {
		t.Errorf("loadQueries refused a file written before the model field existed: %v", err)
	}
}

// TestSubcommandsRefuseALeftoverArgument: flag stops parsing at the first non-flag
// argument and says nothing, so `weft-eval prepare typo -limit=1` leaves -limit at its
// default — and the default here means unlimited, i.e. hours of rate-limited API
// requests appending to the resumable cache, with the flag that was meant to keep it
// small silently discarded.
func TestSubcommandsRefuseALeftoverArgument(t *testing.T) {
	for _, cmd := range []struct {
		name string
		fn   func(context.Context, []string) error
	}{
		{"prepare", prepare}, {"build", build}, {"run", run}, {"sweep", sweep},
		{"weights", weights}, {"diagnose", diagnose},
	} {
		t.Run(cmd.name, func(t *testing.T) {
			err := cmd.fn(context.Background(), []string{"typo", "-data", t.TempDir(), "-any-snapshot"})
			if err == nil || !strings.Contains(err.Error(), "typo") {
				t.Errorf("error = %v, want it to name the argument that stopped flag parsing", err)
			}
		})
	}
}

// TestBuildLeavesNoProvenanceForAnIndexItDidNotFinish closes the window between the
// commit and the record of what was committed.
//
// A rebuild replaces the segments and then writes provenance.json. A crash in between
// used to leave the previous record standing beside a manifest it does not describe,
// and a later run would verify that stale record and accept a foreign or partial index
// as the pinned one — the substitution provenance exists to refuse, reached through
// provenance itself. The old record is removed first, so an unfinished rebuild leaves
// an index that cannot say what it holds, which is what verifyProvenance refuses.
func TestBuildLeavesNoProvenanceForAnIndexItDidNotFinish(t *testing.T) {
	dir := evalDir(t, map[string]string{
		corpusFile: twoDocCorpus,
		s2File:     `{"key":"a","corpus_id":"1"}` + "\n" + `{"key":"b","corpus_id":"2"}` + "\n",
	})
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot"}); err != nil {
		t.Fatalf("first build: %v", err)
	}
	path := filepath.Join(dir, indexDir, provenanceFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("first build wrote no provenance: %v", err)
	}

	// A rebuild that cannot replace the index. The data directory is made
	// unwritable, so clearing the previous index fails — a build that has
	// already removed the provenance record and then gets no further, which is
	// the window this test is about.
	//
	// A corrupt MANIFEST used to serve here: engine refused to commit on top of
	// a directory in an unknown state. It no longer reaches that refusal,
	// because a commit is incremental now and build clears the index directory
	// before committing into it — the previous manifest is gone before engine
	// sees it. The property is unchanged; the way to trip it is not.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) //nolint:errcheck // teardown
	if err := os.Remove(filepath.Join(dir, indexDir, provenanceFile)); err == nil {
		// Running as a user the mode does not constrain — root, typically.
		t.Skip("the data directory is writable despite mode 0500")
	}
	if err := build(context.Background(), []string{"-data", dir, "-any-snapshot"}); err == nil {
		t.Fatal("build replaced an index it could not clear")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("provenance survived a rebuild that did not finish (stat err %v); a later run "+
			"would verify it and publish whatever the index now holds", err)
	}
}

// TestLoadQueriesRejectsJudgmentsForAMissingQuery is the pairing check in the
// direction the loop cannot see.
//
// Judgments are looked up per query, so a qrels row for a query missing from
// queries.jsonl is never reached — not skipped, not counted, not reported. A truncated
// or mismatched query file then produces a mean and a bootstrap over whatever queries
// survived, printed under the usual heading with a query count nobody compares to 50.
// Dropping a judgment-less query is the deliberate case and is counted; this is its
// mirror image, and there is no reading of it that is not a broken pairing.
func TestLoadQueriesRejectsJudgmentsForAMissingQuery(t *testing.T) {
	files := map[string]string{
		queriesFile: `{"_id":"1","text":"what is the origin of COVID-19"}` + "\n",
		qrelsFile:   "query-id\tcorpus-id\tscore\n1\ta\t2\n2\tb\t1\n",
	}
	_, err := loadQueries(evalDir(t, files))
	if err == nil {
		t.Fatal("loadQueries accepted judgments for a query the query file does not hold; " +
			"every arm would be averaged over the queries that happen to remain")
	}
	if !strings.Contains(err.Error(), `"2"`) {
		t.Errorf("error = %v, want it to name the query whose judgments are unreachable", err)
	}

	// The reverse is the deliberate case and stays allowed: a query with no judgments
	// scores 0 by definition, so it is dropped and counted rather than refused.
	files[queriesFile] += `{"_id":"2","text":"a question nobody judged"}` + "\n"
	files[qrelsFile] = "query-id\tcorpus-id\tscore\n1\ta\t2\n"
	qs, err := loadQueries(evalDir(t, files))
	if err != nil {
		t.Fatalf("loadQueries with an unjudged query: %v", err)
	}
	if len(qs) != 1 || qs[0].ID != "1" {
		t.Errorf("loaded %d queries, want only the judged one", len(qs))
	}
}
