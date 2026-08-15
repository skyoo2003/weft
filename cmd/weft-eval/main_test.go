package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyoo2003/weft/internal/eval"
	"github.com/skyoo2003/weft/pkg/engine"
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

	cache, err := scanS2Cache(path)
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

	cache, err := scanS2Cache(path)
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
	again, err := scanS2Cache(path)
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

	cache, err := scanS2Cache(path)
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
	if _, err := scanS2Cache(tail); err == nil {
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
	cache, err := scanS2Cache(filepath.Join(dir, s2File))
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

	cache, err := scanS2Cache(path)
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
