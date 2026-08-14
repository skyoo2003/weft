package eval

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestReadCorpus(t *testing.T) {
	path := writeFile(t, "corpus.jsonl", `{"_id": "d1", "title": "Alpha", "text": "first"}
{"_id": "d2", "title": "", "text": "second"}

{"_id": "d3", "title": "Gamma", "text": "third", "metadata": {"ignored": 1}}
`)

	var got []CorpusDoc
	if err := ReadCorpus(path, func(d CorpusDoc) error {
		got = append(got, d)
		return nil
	}); err != nil {
		t.Fatalf("ReadCorpus: %v", err)
	}

	want := []CorpusDoc{
		{ID: "d1", Title: "Alpha", Text: "first"},
		{ID: "d2", Title: "", Text: "second"},
		{ID: "d3", Title: "Gamma", Text: "third"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d docs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("doc %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestReadCorpusStopsOnCallbackError matters because the callback is where
// engine.Add runs: a rejected document has to abort the build rather than be
// counted as read.
func TestReadCorpusStopsOnCallbackError(t *testing.T) {
	path := writeFile(t, "corpus.jsonl", `{"_id": "d1", "text": "a"}
{"_id": "d2", "text": "b"}
`)
	sentinel := errors.New("index full")
	seen := 0
	err := ReadCorpus(path, func(CorpusDoc) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want %v", err, sentinel)
	}
	if seen != 1 {
		t.Errorf("callback ran %d times, want 1", seen)
	}
}

func TestReadCorpusRejectsBadData(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    error
	}{
		{"not json", "{oops\n", ErrBadRecord},
		{"no id", `{"title": "t", "text": "x"}` + "\n", ErrMissingID},
		{"empty file", "", ErrEmptyDataset},
		{"only blank lines", "\n\n  \n", ErrEmptyDataset},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, "corpus.jsonl", tc.content)
			err := ReadCorpus(path, func(CorpusDoc) error { return nil })
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadCorpusMissingFile(t *testing.T) {
	err := ReadCorpus(filepath.Join(t.TempDir(), "absent.jsonl"), func(CorpusDoc) error { return nil })
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want os.ErrNotExist", err)
	}
}

// TestReadCorpusHandlesLongLines guards the reason scanLines raises bufio's limit:
// a full abstract exceeds the 64 KiB default, and the default failure is a
// partially consumed line.
func TestReadCorpusHandlesLongLines(t *testing.T) {
	long := make([]byte, 200_000)
	for i := range long {
		long[i] = 'a'
	}
	path := writeFile(t, "corpus.jsonl", `{"_id": "d1", "text": "`+string(long)+`"}`+"\n")

	var got CorpusDoc
	if err := ReadCorpus(path, func(d CorpusDoc) error { got = d; return nil }); err != nil {
		t.Fatalf("ReadCorpus: %v", err)
	}
	if len(got.Text) != len(long) {
		t.Errorf("text is %d bytes, want %d", len(got.Text), len(long))
	}
}

func TestReadQueries(t *testing.T) {
	path := writeFile(t, "queries.jsonl", `{"_id": "1", "text": "what is the origin", "metadata": {"query": "origin"}}
{"_id": "2", "text": "how does it spread"}
`)
	got, err := ReadQueries(path)
	if err != nil {
		t.Fatalf("ReadQueries: %v", err)
	}
	want := []EvalQuery{{ID: "1", Text: "what is the origin"}, {ID: "2", Text: "how does it spread"}}
	if len(got) != len(want) {
		t.Fatalf("got %d queries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("query %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReadQueriesRejectsDuplicateID(t *testing.T) {
	path := writeFile(t, "queries.jsonl", `{"_id": "1", "text": "a"}
{"_id": "1", "text": "b"}
`)
	if _, err := ReadQueries(path); !errors.Is(err, ErrDuplicateQ) {
		t.Errorf("error = %v, want ErrDuplicateQ", err)
	}
}

func TestReadQrels(t *testing.T) {
	path := writeFile(t, "test.tsv", "query-id\tcorpus-id\tscore\n1\td1\t2\n1\td2\t0\n2\td3\t1\n")
	got, err := ReadQrels(path)
	if err != nil {
		t.Fatalf("ReadQrels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d queries, want 2", len(got))
	}
	if got["1"]["d1"] != 2 || got["1"]["d2"] != 0 || got["2"]["d3"] != 1 {
		t.Errorf("qrels = %v", got)
	}
	// A judged 0 must be present as a key. NDCG treats it the same as unjudged, but
	// coverage reporting counts judgments, and dropping the zeros would understate
	// how deeply a query was judged.
	if _, ok := got["1"]["d2"]; !ok {
		t.Error("a judged grade of 0 was dropped instead of stored")
	}
}

// TestReadQrelsResolvesColumnsByName is the case that motivated reading the header
// at all: a reordered file parses silently if the header is skipped, and every
// judgment then attaches to the wrong query.
func TestReadQrelsResolvesColumnsByName(t *testing.T) {
	path := writeFile(t, "test.tsv", "corpus-id\tscore\tquery-id\nd1\t2\t1\n")
	got, err := ReadQrels(path)
	if err != nil {
		t.Fatalf("ReadQrels: %v", err)
	}
	if got["1"]["d1"] != 2 {
		t.Errorf("qrels = %v, want query 1 -> d1 -> 2", got)
	}
}

func TestReadQrelsRejectsBadData(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    error
	}{
		{"missing header column", "query-id\tcorpus-id\n1\td1\n", ErrBadRecord},
		{"short row", "query-id\tcorpus-id\tscore\n1\td1\n", ErrBadRecord},
		{"non-numeric score", "query-id\tcorpus-id\tscore\n1\td1\thigh\n", ErrBadRecord},
		{"empty id", "query-id\tcorpus-id\tscore\n1\t\t2\n", ErrMissingID},
		{"header only", "query-id\tcorpus-id\tscore\n", ErrEmptyDataset},
		{"empty file", "", ErrEmptyDataset},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, "test.tsv", tc.content)
			if _, err := ReadQrels(path); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadQueryVectors(t *testing.T) {
	path := writeFile(t, "query-vectors.jsonl", `{"id": "1", "text": "a", "vec": [0.1, -0.2]}

{"id": "2", "text": "b", "vec": [1, 2]}
`)
	got, err := ReadQueryVectors(path)
	if err != nil {
		t.Fatalf("ReadQueryVectors: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d vectors, want 2", len(got))
	}
	if want := []float32{0.1, -0.2}; !slices.Equal(got["1"].Vec, want) {
		t.Errorf(`got["1"].Vec = %v, want %v`, got["1"].Vec, want)
	}
	// The text is kept, not discarded. It is the only field that distinguishes a
	// vector generated for this query from one generated for whatever question wore
	// this id in an earlier snapshot of the query set.
	if got["1"].Text != "a" {
		t.Errorf(`got["1"].Text = %q, want "a"`, got["1"].Text)
	}
}

// TestReadQueryVectorsCannotProduceNonFinite pins the reason there is no explicit
// non-finite guard here while parseS2Batch has one. Decoding straight into
// []float32 makes encoding/json apply the range check, so the value that becomes
// +Inf when narrowed by hand never gets that far. If this ever starts passing as a
// finite vector, the guard has to come back.
func TestReadQueryVectorsCannotProduceNonFinite(t *testing.T) {
	// 1e300 is a valid finite float64 and out of range for float32.
	// Text supplied so the rejection can only be the range check: without it the
	// record would be refused as unverifiable and the test would pass for the wrong
	// reason, with the same sentinel error.
	path := writeFile(t, "query-vectors.jsonl", `{"id": "1", "text": "a", "vec": [1e300, 2]}`+"\n")
	got, err := ReadQueryVectors(path)
	if !errors.Is(err, ErrBadRecord) {
		t.Fatalf("error = %v (vectors %v), want ErrBadRecord from encoding/json's range check", err, got)
	}
}

func TestReadQueryVectorsRejectsBadData(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    error
	}{
		{"not json", "{oops\n", ErrBadRecord},
		{"no id", `{"vec": [1, 2]}` + "\n", ErrMissingID},
		{"empty vector", `{"id": "1", "text": "a", "vec": []}` + "\n", ErrBadRecord},
		{"no vector field", `{"id": "1", "text": "a"}` + "\n", ErrBadRecord},
		// No text means the file cannot be checked against the query set at all, and
		// ids are too stable to pair on alone.
		{"no text", `{"id": "1", "vec": [1, 2]}` + "\n", ErrBadRecord},
		// Present, well-formed, and no opinion: the vector scorer reads a zero norm as
		// an abstention, so this file reports full coverage for a text+vector arm that
		// is exactly text.
		{"all-zero vector", `{"id": "1", "text": "a", "vec": [0, 0, 0]}` + "\n", ErrBadRecord},
		{"all-zero vector, signed", `{"id": "1", "text": "a", "vec": [0, -0.0]}` + "\n", ErrBadRecord},
		{"empty file", "", ErrEmptyDataset},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, "query-vectors.jsonl", tc.content)
			if _, err := ReadQueryVectors(path); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestReadQueryVectorsMissingFileIsDistinguishable: run treats a missing file as
// "no vector arm, say so loudly" and anything else as fatal, so the two must not
// arrive as the same error.
func TestReadQueryVectorsMissingFileIsDistinguishable(t *testing.T) {
	_, err := ReadQueryVectors(filepath.Join(t.TempDir(), "absent.jsonl"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want os.ErrNotExist", err)
	}
}

func TestReadCORD19IDsPrefersS2ID(t *testing.T) {
	path := writeFile(t, "metadata.csv",
		"cord_uid,doi,pmcid,pubmed_id,s2_id\n"+
			"d1,10.1000/one,PMC1,111,999\n"+ // all four present: s2_id wins
			"d2,10.1000/two,PMC2,222,\n"+ // no s2_id: doi wins
			"d3,,PMC3,333,\n"+ // no doi: pmcid wins
			"d4,,,444,\n"+ // only pubmed_id
			"d5,,,,\n"+ // nothing usable
			"d6,10.1000/six,,,\n") // not requested

	want := map[string]bool{"d1": true, "d2": true, "d3": true, "d4": true, "d5": true}
	got, tally, err := ReadCORD19IDs(path, want)
	if err != nil {
		t.Fatalf("ReadCORD19IDs: %v", err)
	}

	expect := map[string]ExternalIDs{
		"d1": {S2Ref: "CorpusId:999", Source: "s2_id"},
		"d2": {S2Ref: "DOI:10.1000/two", Source: "doi"},
		"d3": {S2Ref: "PMCID:PMC3", Source: "pmcid"},
		"d4": {S2Ref: "PMID:444", Source: "pubmed_id"},
	}
	if len(got) != len(expect) {
		t.Fatalf("got %d ids (%v), want %d", len(got), got, len(expect))
	}
	for k, v := range expect {
		if got[k] != v {
			t.Errorf("%s = %+v, want %+v", k, got[k], v)
		}
	}
	if _, ok := got["d5"]; ok {
		t.Error("d5 has no usable identifier but was returned anyway")
	}
	if _, ok := got["d6"]; ok {
		t.Error("d6 was not requested but was returned")
	}
	if tally["s2_id"] != 1 || tally["doi"] != 1 || tally["pmcid"] != 1 || tally["pubmed_id"] != 1 {
		t.Errorf("tally = %v, want one of each column", tally)
	}
}

func TestReadCORD19IDsTakesFirstOfMultipleDOIs(t *testing.T) {
	path := writeFile(t, "metadata.csv", "cord_uid,doi\nd1,\"10.1000/first; 10.1000/second\"\n")
	got, _, err := ReadCORD19IDs(path, map[string]bool{"d1": true})
	if err != nil {
		t.Fatalf("ReadCORD19IDs: %v", err)
	}
	if got["d1"].S2Ref != "DOI:10.1000/first" {
		t.Errorf("d1 = %q, want DOI:10.1000/first", got["d1"].S2Ref)
	}
}

func TestReadCORD19IDsFirstRowWins(t *testing.T) {
	path := writeFile(t, "metadata.csv", "cord_uid,s2_id\nd1,111\nd1,222\n")
	got, _, err := ReadCORD19IDs(path, map[string]bool{"d1": true})
	if err != nil {
		t.Fatalf("ReadCORD19IDs: %v", err)
	}
	if got["d1"].S2Ref != "CorpusId:111" {
		t.Errorf("d1 = %q, want CorpusId:111", got["d1"].S2Ref)
	}
}

// TestReadCORD19IDsRejectsReleaseWithoutJoinColumn is the check the plan called for
// by name: these columns differ between CORD-19 releases, so a release we cannot
// join to must fail loudly rather than yield an empty graph.
func TestReadCORD19IDsRejectsReleaseWithoutJoinColumn(t *testing.T) {
	path := writeFile(t, "metadata.csv", "cord_uid,title,abstract\nd1,Alpha,text\n")
	_, _, err := ReadCORD19IDs(path, map[string]bool{"d1": true})
	if !errors.Is(err, ErrNoJoinColumn) {
		t.Errorf("error = %v, want ErrNoJoinColumn", err)
	}
}

func TestReadCORD19IDsRejectsMissingUIDColumn(t *testing.T) {
	path := writeFile(t, "metadata.csv", "id,doi\nd1,10.1000/x\n")
	_, _, err := ReadCORD19IDs(path, map[string]bool{"d1": true})
	if !errors.Is(err, ErrBadRecord) {
		t.Errorf("error = %v, want ErrBadRecord", err)
	}
}

func TestReadCORD19IDsNoMatch(t *testing.T) {
	path := writeFile(t, "metadata.csv", "cord_uid,s2_id\nother,111\n")
	_, _, err := ReadCORD19IDs(path, map[string]bool{"d1": true})
	if !errors.Is(err, ErrEmptyDataset) {
		t.Errorf("error = %v, want ErrEmptyDataset", err)
	}
}

// TestReadCORD19IDsToleratesRaggedRows covers LazyQuotes and the variable column
// count: the real file has both, and aborting a 1.6 GB scan on one bad row would
// throw away the join over a malformed abstract.
func TestReadCORD19IDsToleratesRaggedRows(t *testing.T) {
	path := writeFile(t, "metadata.csv",
		"cord_uid,abstract,s2_id\n"+
			"d1,\"a \"quoted\" mess\",111\n"+
			"d2\n"+ // short row, no s2_id column at all
			"d3,fine,333\n")
	got, tally, err := ReadCORD19IDs(path, map[string]bool{"d1": true, "d2": true, "d3": true})
	if err != nil {
		t.Fatalf("ReadCORD19IDs: %v", err)
	}
	if got["d3"].S2Ref != "CorpusId:333" {
		t.Errorf("d3 = %q, want CorpusId:333 — a ragged earlier row stopped the scan", got["d3"].S2Ref)
	}
	if _, ok := got["d2"]; ok {
		t.Error("d2 had no identifier column and should not be returned")
	}
	t.Logf("tally: %v", tally)
}
