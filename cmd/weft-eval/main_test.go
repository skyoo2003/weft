package main

import (
	"context"
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

	err := prepare(context.Background(), []string{"-data", dir})
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
	if err := prepare(context.Background(), []string{"-data", dir}); err != nil {
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

	err := build(context.Background(), []string{"-data", dir})
	if err == nil {
		t.Fatal("build succeeded on a cache covering 1 of 2 documents; it must refuse, " +
			"or half the corpus enters the index text-only under a text+vector+graph label")
	}
	if !strings.Contains(err.Error(), "-partial") {
		t.Errorf("error = %v, want it to name the -partial opt-in", err)
	}

	// The same build is allowed when it is asked for, because smoke-testing the
	// pipeline on a slice is a real use — it just must not be the default.
	if err := build(context.Background(), []string{"-data", dir, "-partial"}); err != nil {
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
	if err := build(context.Background(), []string{"-data", dir}); err != nil {
		t.Fatalf("build over a complete tombstone-only cache: %v", err)
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
