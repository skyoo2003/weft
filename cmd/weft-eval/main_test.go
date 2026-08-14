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

	done, _, good, err := doneKeys(path)
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

	done, _, good, err := doneKeys(path)
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
	again, _, _, err := doneKeys(path)
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

// TestDoneKeysTalliesTheModelsAlreadyCached is the resume regression for embedding
// provenance. The model tally used to live only in the invocation that fetched a
// batch, so a prepare that died mid-run took the provenance of everything it had
// written with it, and the run that finished the job reported only its own tail as
// clean. Two embedding spaces in one cache would then reach build with nothing said.
func TestDoneKeysTalliesTheModelsAlreadyCached(t *testing.T) {
	path := writeCache(t, strings.Join([]string{
		`{"key":"a","vec":[1,2],"model":"specter_v2"}`,
		`{"key":"b","vec":[1,2],"model":"specter_v1"}`,
		`{"key":"c","vec":[1,2]}`, // written before the field existed
		`{"key":"d"}`,             // tombstone: no vector, so no embedding space
		"",
	}, "\n"))

	_, models, _, err := doneKeys(path)
	if err != nil {
		t.Fatalf("doneKeys: %v", err)
	}
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
