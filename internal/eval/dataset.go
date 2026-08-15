package eval

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Sentinel errors from the dataset readers.
//
// Every one of them exists because the alternative is a silently smaller corpus.
// A truncated qrels file, a corpus line that failed to parse, or a metadata
// release missing the column we join on all produce a run that completes and
// reports a lower nDCG — and nothing distinguishes that from a ranking that got
// worse. Data files are a trust boundary exactly like Index.Add's arguments are.
var (
	ErrEmptyDataset = errors.New("eval: dataset file contained no records")
	ErrBadRecord    = errors.New("eval: malformed record")
	ErrMissingID    = errors.New("eval: record has no id")

	// ErrDuplicateDoc reports a corpus that names the same document twice. Refused
	// at the read rather than downstream, because every consumer reaches a different
	// wrong answer from it and the cheapest of them is expensive: prepare puts the
	// key in its fetch list twice, asks Semantic Scholar about it twice, and writes
	// two cache records under one key — which build rejects, after the hours of
	// rate-limited fetching that produced them.
	ErrDuplicateDoc = errors.New("eval: duplicate corpus document id")

	// ErrNoJoinColumn reports a CORD-19 release whose header carries none of the
	// identifier columns we can join to Semantic Scholar on. The plan calls this
	// out specifically: these columns differ between releases, so the join has to
	// verify rather than assume.
	ErrNoJoinColumn = errors.New("eval: metadata has no usable Semantic Scholar identifier column")
)

// scanLines returns a Scanner sized for BEIR corpus lines. A full-text abstract
// well exceeds bufio's 64 KiB default, and the default failure mode is
// Scanner.Err reporting a long line after a prefix has already been consumed.
func scanLines(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return s
}

// CorpusDoc is one BEIR corpus record.
type CorpusDoc struct {
	ID    string
	Title string
	Text  string
}

// ReadCorpus streams path, a BEIR corpus.jsonl, calling fn for each record.
//
// Streaming rather than returning a slice: trec-covid is 171K documents and the
// caller's next move is to put each one into an index, so materialising the whole
// corpus first would hold two copies of it. An error from fn aborts the read.
//
// The ids are held even so, and only the ids: a repeated one is refused here the way
// ReadQueries refuses a repeated query id. It is the same class of fault and it is
// the more expensive one — nothing downstream is in a position to say which line the
// duplicate came from, and prepare's response to seeing the key twice is to spend a
// second rate-limited fetch on it. A set of 171K keys is a few megabytes beside the
// documents themselves, which is what the streaming above is protecting.
func ReadCorpus(path string, fn func(CorpusDoc) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open corpus: %w", err)
	}
	defer f.Close()

	n := 0
	seen := make(map[string]struct{})
	s := scanLines(f)
	for line := 1; s.Scan(); line++ {
		raw := s.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var rec struct {
			ID    string `json:"_id"`
			Title string `json:"title"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("%s line %d: %w: %v", path, line, ErrBadRecord, err)
		}
		if rec.ID == "" {
			return fmt.Errorf("%s line %d: %w", path, line, ErrMissingID)
		}
		if _, dup := seen[rec.ID]; dup {
			return fmt.Errorf("%s line %d: %q: %w", path, line, rec.ID, ErrDuplicateDoc)
		}
		seen[rec.ID] = struct{}{}
		if err := fn(CorpusDoc{ID: rec.ID, Title: rec.Title, Text: rec.Text}); err != nil {
			return fmt.Errorf("%s line %d: doc %q: %w", path, line, rec.ID, err)
		}
		n++
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", path, ErrEmptyDataset)
	}
	return nil
}

// EvalQuery is one BEIR query record, before qrels are attached.
type EvalQuery struct {
	ID   string
	Text string
}

// ReadQueries reads a BEIR queries.jsonl. Returned as a slice: trec-covid has 50.
func ReadQueries(path string) ([]EvalQuery, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open queries: %w", err)
	}
	defer f.Close()

	var out []EvalQuery
	seen := make(map[string]bool)
	s := scanLines(f)
	for line := 1; s.Scan(); line++ {
		raw := s.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var rec struct {
			ID   string `json:"_id"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("%s line %d: %w: %v", path, line, ErrBadRecord, err)
		}
		if rec.ID == "" {
			return nil, fmt.Errorf("%s line %d: %w", path, line, ErrMissingID)
		}
		// Evaluate would reject a duplicate id later, but reporting it here names
		// the file and line that produced it.
		if seen[rec.ID] {
			return nil, fmt.Errorf("%s line %d: %q: %w", path, line, rec.ID, ErrDuplicateQ)
		}
		seen[rec.ID] = true
		out = append(out, EvalQuery{ID: rec.ID, Text: rec.Text})
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: %w", path, ErrEmptyDataset)
	}
	return out, nil
}

// ReadQrels reads a BEIR qrels TSV into query id -> document id -> grade.
//
// The header is required and resolved by name rather than skipped by position. A
// qrels file whose columns are ordered differently parses without complaint if the
// header is merely discarded, and the result is every judgment attached to the
// wrong query — a number that looks plausible and means nothing.
func ReadQrels(path string) (map[string]map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open qrels: %w", err)
	}
	defer f.Close()

	s := scanLines(f)
	if !s.Scan() {
		if err := s.Err(); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		return nil, fmt.Errorf("%s: %w", path, ErrEmptyDataset)
	}
	head := strings.Split(strings.TrimSpace(s.Text()), "\t")
	col := make(map[string]int, len(head))
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	qi, ok1 := col["query-id"]
	di, ok2 := col["corpus-id"]
	si, ok3 := col["score"]
	if !ok1 || !ok2 || !ok3 {
		return nil, fmt.Errorf("%s: header is %v, want query-id, corpus-id and score: %w",
			path, head, ErrBadRecord)
	}
	widest := max(qi, max(di, si))

	out := make(map[string]map[string]int)
	rows := 0
	for line := 2; s.Scan(); line++ {
		text := strings.TrimSpace(s.Text())
		if text == "" {
			continue
		}
		cells := strings.Split(text, "\t")
		if len(cells) <= widest {
			return nil, fmt.Errorf("%s line %d: %d columns, need %d: %w",
				path, line, len(cells), widest+1, ErrBadRecord)
		}
		grade, err := strconv.Atoi(strings.TrimSpace(cells[si]))
		if err != nil {
			return nil, fmt.Errorf("%s line %d: score %q: %w", path, line, cells[si], ErrBadRecord)
		}
		qid, did := strings.TrimSpace(cells[qi]), strings.TrimSpace(cells[di])
		if qid == "" || did == "" {
			return nil, fmt.Errorf("%s line %d: %w", path, line, ErrMissingID)
		}
		if out[qid] == nil {
			out[qid] = make(map[string]int)
		}
		// A repeated pair carrying a different grade is an ambiguous file, not a later
		// revision winning. Assigning would keep whichever row the scanner happened to
		// reach last, so concatenating two assessment rounds — the way this actually
		// happens — silently makes the ground truth, and therefore every nDCG computed
		// against it, a function of row order. An identical repeat is left alone: it
		// says the same thing twice and there is nothing to choose between.
		if prev, dup := out[qid][did]; dup && prev != grade {
			return nil, fmt.Errorf("%s line %d: query %s document %s is graded %d here and %d "+
				"earlier in the same file, so the judgments depend on row order: %w",
				path, line, qid, did, grade, prev, ErrBadRecord)
		}
		out[qid][did] = grade
		rows++
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if rows == 0 {
		return nil, fmt.Errorf("%s: %w", path, ErrEmptyDataset)
	}
	return out, nil
}

// QueryVector is one line of query-vectors.jsonl: the embedding, and the query
// text it was computed from.
//
// Text is carried rather than discarded because the id is not enough to pair a
// vector with a query. Ids are short and stable across regenerations of the query
// set — trec-covid numbers them "1" to "50" — so a file generated from an older
// snapshot pairs cleanly by id and evaluates embeddings of different questions
// under the current qrels, with nothing wrong in the coverage count to see. The
// text is the only field that changes when the question does.
type QueryVector struct {
	Text string
	Vec  []float32

	// Model is the embedding the vector came out of, as gen_query_vectors.py records
	// it. Empty means a file written before the field existed — which is what the
	// committed measurement was generated into — so absence is tolerated and a
	// mismatch is not; see loadQueries, and checkVectorModels for the document side of
	// the same hazard.
	Model string
}

// ReadQueryVectors reads a query-vectors.jsonl into query id -> vector.
//
// Produced by testdata/gen_query_vectors.py; see that script for which model and
// why the pairing with the document vectors has to be verified rather than assumed.
// This function checks that a record is usable at all; whether it belongs to *this*
// query set is the caller's check, against the text (cmd/weft-eval loadQueries).
//
// Two ways a vector can be present and still carry no opinion, both rejected here
// rather than downstream, where they are indistinguishable from a vector scorer
// that honestly ranked nothing: empty, and all zero. The all-zero case is the
// quieter one — pkg/scorer/vector treats a zero norm as no opinion because cosine
// is undefined there, so full coverage gets reported for a file under which the
// text+vector arm is exactly text, walking straight past the all-or-none check in
// loadQueries that exists to catch that.
//
// There is no non-finite guard here, unlike the one on document vectors in
// parseS2Batch, and the difference is worth stating because a missing guard usually
// means an oversight. That one decodes into []float64 and narrows by hand, so an
// ordinary JSON value like 1e300 survives decoding and becomes +Inf on the way to
// float32. This one decodes straight into []float32, which makes encoding/json do
// the range check: 1e300 is rejected as ErrBadRecord before any component exists.
// JSON has no NaN or Infinity literal, so that closes the whole hole.
//
// It matters because a NaN query vector would be worse than a rejected one — every
// cosine becomes NaN, TopK sorts NaN last, and the vector scorer would hand fusion
// a confident stream of the corpus's highest DocIDs.
func ReadQueryVectors(path string) (map[string]QueryVector, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open query vectors: %w", err)
	}
	defer f.Close()

	out := make(map[string]QueryVector)
	s := scanLines(f)
	for line := 1; s.Scan(); line++ {
		raw := s.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var rec struct {
			ID    string    `json:"id"`
			Text  string    `json:"text"`
			Vec   []float32 `json:"vec"`
			Model string    `json:"model"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("%s line %d: %w: %v", path, line, ErrBadRecord, err)
		}
		if rec.ID == "" {
			return nil, fmt.Errorf("%s line %d: %w", path, line, ErrMissingID)
		}
		// A record with no text cannot be checked against the query set at all, so it
		// is refused rather than admitted as an unverifiable vector. gen_query_vectors.py
		// has always written this field; a file missing it did not come from that script.
		if rec.Text == "" {
			return nil, fmt.Errorf("%s line %d: query %q has no text to verify the pairing against: %w "+
				"(regenerate with testdata/gen_query_vectors.py)", path, line, rec.ID, ErrBadRecord)
		}
		if len(rec.Vec) == 0 {
			return nil, fmt.Errorf("%s line %d: query %q has an empty vector: %w",
				path, line, rec.ID, ErrBadRecord)
		}
		nonzero := false
		for _, v := range rec.Vec {
			if v != 0 {
				nonzero = true
				break
			}
		}
		if !nonzero {
			return nil, fmt.Errorf("%s line %d: query %q has an all-zero vector, which the vector "+
				"scorer reads as no opinion: %w", path, line, rec.ID, ErrBadRecord)
		}
		// Rejected outright, unlike a repeated qrels row: gen_query_vectors.py writes
		// exactly one line per query, so a second line for one id means the file was
		// assembled from more than one generation. Both lines then carry the same query
		// text, so the pairing check in loadQueries cannot tell them apart and neither
		// can the all-or-none coverage count — whichever came last is the vector arm
		// that gets measured, and comparing the two embeddings would only answer
		// whether the file is ambiguous, which is already known.
		if _, dup := out[rec.ID]; dup {
			return nil, fmt.Errorf("%s line %d: query %q already has a vector earlier in the file: %w "+
				"(two generations concatenated? regenerate with testdata/gen_query_vectors.py)",
				path, line, rec.ID, ErrBadRecord)
		}
		out[rec.ID] = QueryVector{Text: rec.Text, Vec: rec.Vec, Model: rec.Model}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: %w", path, ErrEmptyDataset)
	}
	return out, nil
}

// ExternalIDs is the identifier for one CORD-19 document, in the form Semantic
// Scholar's batch endpoint accepts.
type ExternalIDs struct {
	// S2Ref is the best available lookup string, already prefixed —
	// "CorpusId:215736195", "DOI:10.1000/x", "PMCID:PMC7095448" or
	// "PMID:32074550". Empty means the document cannot be looked up at all.
	S2Ref string

	// Source names the column S2Ref came from, so coverage can be reported per
	// identifier kind instead of as one number.
	Source string
}

// cord19JoinColumns are the metadata.csv columns that can address a paper in
// Semantic Scholar, best first.
//
// s2_id first because it is the CorpusId — an exact internal key with no
// resolution step and no ambiguity. DOI next, being near-universal in this
// corpus. The PubMed identifiers last: present for a smaller share, and a
// preprint that later acquired a PMID can resolve to the published version rather
// than the indexed preprint.
var cord19JoinColumns = []struct {
	column string
	prefix string
}{
	{"s2_id", "CorpusId:"},
	{"doi", "DOI:"},
	{"pmcid", "PMCID:"},
	{"pubmed_id", "PMID:"},
}

// ReadCORD19IDs streams a CORD-19 metadata.csv and returns identifiers for the
// cord_uids in want, along with a per-column tally of where they came from.
//
// want is required rather than optional. The 2022-06-02 release is 1.6 GB across
// more than a million rows while the corpus being indexed is a fraction of that,
// so filtering during the scan keeps the returned map proportional to what will
// actually be used.
//
// The header is inspected before any row is read, and a release carrying none of
// the join columns is an error rather than an empty result — the plan's explicit
// instruction, because these columns differ between releases.
func ReadCORD19IDs(path string, want map[string]bool) (map[string]ExternalIDs, map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open metadata: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReaderSize(f, 1<<20))
	// Abstracts in this file contain unescaped quotes in some rows, and the column
	// count varies across releases. Both are the file's problem, not a reason to
	// abandon a 1.6 GB scan.
	r.LazyQuotes = true
	r.FieldsPerRecord = -1

	head, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("read %s header: %w", path, err)
	}
	col := make(map[string]int, len(head))
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	uidCol, ok := col["cord_uid"]
	if !ok {
		return nil, nil, fmt.Errorf("%s: header is %v, want a cord_uid column: %w",
			path, head, ErrBadRecord)
	}

	type joiner struct {
		idx    int
		column string
		prefix string
	}
	var joiners []joiner
	for _, jc := range cord19JoinColumns {
		if i, ok := col[jc.column]; ok {
			joiners = append(joiners, joiner{idx: i, column: jc.column, prefix: jc.prefix})
		}
	}
	if len(joiners) == 0 {
		return nil, nil, fmt.Errorf("%s: header is %v: %w", path, head, ErrNoJoinColumn)
	}

	out := make(map[string]ExternalIDs, len(want))
	tally := make(map[string]int, len(joiners)+2)
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// One unparseable row in a corpus this size is not worth aborting for,
			// but it is worth counting: a large tally here is the signal that
			// LazyQuotes is papering over a real format change.
			//
			// A parse error only. csv.Reader has consumed the line it failed on, so
			// skipping it makes progress; a read error consumed nothing and comes back
			// identically on the next call, so counting that as a skippable row spins
			// this loop forever over a 1.6 GB file — silently, in a command already
			// measured in hours, with nothing but CPU to show for it.
			var parse *csv.ParseError
			if !errors.As(err, &parse) {
				return nil, nil, fmt.Errorf("read %s: %w", path, err)
			}
			tally["unreadable-row"]++
			continue
		}
		if uidCol >= len(row) {
			tally["short-row"]++
			continue
		}
		uid := strings.TrimSpace(row[uidCol])
		if uid == "" || !want[uid] {
			continue
		}
		// The first row that yielded an identifier wins. metadata.csv repeats a
		// cord_uid across releases of the same paper and later rows are not reliably
		// better. A row whose join columns were all empty left nothing to keep, so it
		// holds no position and a later row for the same uid still gets to fill it.
		if _, dup := out[uid]; dup {
			continue
		}
		for _, j := range joiners {
			if j.idx >= len(row) {
				continue
			}
			// A cell can hold several identifiers separated by "; " — take the first.
			// Semicolon only: a DOI suffix may legitimately contain a comma, so
			// splitting on one truncates a valid DOI into an unresolvable one, and the
			// paper then drops out of the citation graph with no trace but a bumped
			// unresolved count.
			v := strings.TrimSpace(row[j.idx])
			if i := strings.IndexByte(v, ';'); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			if v == "" {
				continue
			}
			out[uid] = ExternalIDs{S2Ref: j.prefix + v, Source: j.column}
			tally[j.column]++
			break
		}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%s: no requested cord_uid matched: %w", path, ErrEmptyDataset)
	}
	return out, tally, nil
}
