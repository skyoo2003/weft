// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/query"
)

// mappingFile is the mapping's name inside an index directory, beside the
// segments and the _source store.
const mappingFile = "_mapping.json"

// A field type this server can honour, and there are five.
//
// The list is short because each entry has to earn itself twice — once at index
// time, deciding what term a value becomes, and once at query time, deciding
// what a bound in a range clause becomes. A type that did not change either
// answer would be `text` wearing a different name, and a type this server
// accepted and then treated as `text` would make a range query match nothing
// with nothing to report, which is the failure docs/LIMITATIONS.md names and
// D-026 refuses to relocate into a client's dashboard.
const (
	typeText      = "text"
	typeKeyword   = "keyword"
	typeDate      = "date"
	typeInteger   = "integer"
	typeLong      = "long"
	typeKNNVector = "knn_vector"
)

// supportedTypes is the five, in the order an error message should list them.
var supportedTypes = []string{typeText, typeKeyword, typeDate, typeInteger, typeLong, typeKNNVector}

// ErrMappingConflict is a re-mapping of a field that already has a type.
//
// Refused rather than applied. The documents already indexed were encoded by the
// old type's rule — a `long` is sixteen hex digits and a `text` is its own words
// — so changing the rule leaves an index holding two encodings of one field and
// a range query that reads half of it. OpenSearch refuses the same change for the
// same reason.
var ErrMappingConflict = errors.New("opensearch: a field already has a different type")

// property is one field's declaration, as it is written on the wire and on disk.
// Recency is weft's, not OpenSearch's — the same kind of extension `dimension`
// already is for the k-NN plugin. engine.Document.Time is a field the index has
// and a JSON body does not, so something has to say which date is it, and a
// mapping is the only place that knows a field's type at both index and query
// time. scorer/recency reads Document.Time and nothing else.
// Links is the same kind of extension, for the same reason and one signal
// further along: engine.Document.Links is a set of document keys, and on the
// wire that is a JSON array of strings — indistinguishable from any other array
// of strings. scorer/graph reads Links and nothing else.
type property struct {
	Type      string `json:"type"`
	Dimension int    `json:"dimension,omitempty"`
	Recency   bool   `json:"recency,omitempty"`
	Links     bool   `json:"links,omitempty"`
}

// mappingDoc is the file format, and it is also the wire format OpenSearch uses
// for `PUT /{index}` mappings and `GET /{index}/_mapping`. One shape rather than
// two, because a translation layer between them would be a second place for a
// field name to be spelled.
type mappingDoc struct {
	Properties map[string]property `json:"properties"`
}

// Mapping is what this server knows about a field that the index does not.
//
// weft's index holds terms and postings; it does not hold the fact that `views`
// was a number, and PRD section 5 is the whole argument for why somebody has to.
// query.EncodeInt only works when the same rule is applied at index time and at
// query time, and this is the one thing that knows both moments.
//
// It is the server's, not the library's — see D-025. engine.Document is closed
// and stays closed.
type Mapping struct {
	mu    sync.RWMutex
	props map[string]property
}

// NewMapping returns a mapping with no fields declared.
func NewMapping() *Mapping {
	return &Mapping{props: map[string]property{}}
}

// LoadMapping reads the mapping in dir. A directory with no mapping file has no
// declared fields, which is what every index created before this milestone is.
func LoadMapping(dir string) (*Mapping, error) {
	m := NewMapping()
	path := filepath.Join(dir, mappingFile)

	// A constant name under a directory this process owns; no part of the path
	// comes from a request. Registry.validName holds the directory half.
	raw, err := os.ReadFile(path) //nolint:gosec // constant name under an owned directory
	switch {
	case errors.Is(err, os.ErrNotExist):
		return m, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var doc mappingDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s did not decode, so the fields it declares cannot be honoured and treating "+
			"it as empty would silently change how every range query over them reads: repair or remove it "+
			"and re-index: %w", path, err)
	}
	for name, p := range doc.Properties {
		if err := validProperty(name, p); err != nil {
			return nil, fmt.Errorf("%s declares %q: %w", path, name, err)
		}
	}
	if doc.Properties != nil {
		m.props = doc.Properties
	}
	return m, nil
}

// Type reports a field's declared type, or "" when the field is not declared.
func (m *Mapping) Type(field string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.props[field].Type
}

// isVector, isRecency and isLinks are the singleton bindings: the field whose
// value becomes engine.Document.Vector, the one whose value becomes
// Document.Time, and the one whose value becomes Document.Links.
//
// One each per index, because a document carries one of each — so a second
// declaration is one this server could accept and then not honour.
//
// The comment on bound below said the third signal to want one would add a
// predicate and no code. This is that third signal, and it did.
func isVector(p property) bool  { return p.Type == typeKNNVector }
func isRecency(p property) bool { return p.Recency }
func isLinks(p property) bool   { return p.Links }

// bound finds the field a singleton binding names, and it is the same search for
// both of them: the third signal to want one adds a predicate and no code.
func (m *Mapping) bound(is func(property) bool) (field string, p property, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.boundLocked(is)
}

func (m *Mapping) boundLocked(is func(property) bool) (field string, p property, ok bool) {
	for name, prop := range m.props {
		if is(prop) {
			return name, prop, true
		}
	}
	return "", property{}, false
}

// Vector reports the declared knn_vector field and its width.
func (m *Mapping) Vector() (field string, dim int, ok bool) {
	name, p, ok := m.bound(isVector)
	return name, p.Dimension, ok
}

// Recency reports the date field bound to engine.Document.Time.
func (m *Mapping) Recency() (field string, ok bool) {
	name, _, ok := m.bound(isRecency)
	return name, ok
}

// Links reports the keyword field bound to engine.Document.Links.
func (m *Mapping) Links() (field string, ok bool) {
	name, _, ok := m.bound(isLinks)
	return name, ok
}

// doc is the mapping as it goes onto the wire and onto disk.
func (m *Mapping) doc() mappingDoc {
	m.mu.RLock()
	defer m.mu.RUnlock()
	props := make(map[string]property, len(m.props))
	for name, p := range m.props {
		props[name] = p
	}
	return mappingDoc{Properties: props}
}

// merge adds declarations, refusing any that would change a field already
// declared or that this server cannot honour.
//
// All-or-nothing: the incoming set is validated whole before a single field is
// recorded. A partially applied mapping would leave the client's next document
// encoded half by the new rule and half by the old, and the client was told the
// request failed.
func (m *Mapping) merge(doc mappingDoc) error {
	names := make([]string, 0, len(doc.Properties))
	for name := range doc.Properties {
		names = append(names, name)
	}
	slices.Sort(names) // so two identical requests fail on the same field

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, name := range names {
		p := doc.Properties[name]
		if err := validProperty(name, p); err != nil {
			return err
		}
		if old, exists := m.props[name]; exists && old != p {
			return fmt.Errorf("%q is already mapped as %q and the documents holding it were indexed by that "+
				"rule, so re-mapping it to %q would leave the field half readable: create a new index: %w",
				name, old.Type, p.Type, ErrMappingConflict)
		}
		for _, b := range []struct {
			declared bool
			is       func(property) bool
			what     string
		}{
			{isVector(p), isVector, "a knn_vector (a document carries one engine.Document.Vector)"},
			{isRecency(p), isRecency, "the recency field (a document carries one engine.Document.Time)"},
			{isLinks(p), isLinks, "the links field (a document carries one engine.Document.Links)"},
		} {
			if other, _, ok := m.boundLocked(b.is); b.declared && ok && other != name {
				return fmt.Errorf("%q cannot be %s because %q already is", name, b.what, other)
			}
		}
	}
	for _, name := range names {
		m.props[name] = doc.Properties[name]
	}
	return nil
}

// save publishes the mapping into dir.
func (m *Mapping) save(dir string) error {
	raw, err := json.Marshal(m.doc())
	if err != nil {
		return fmt.Errorf("encode the mapping: %w", err)
	}
	return writeAtomic(dir, mappingFile, raw)
}

// validProperty is the whole of what this server will accept as a declaration.
func validProperty(name string, p property) error {
	if name == "" || strings.ContainsRune(name, 0) {
		// engine.ErrBadField's rule, checked here so the client hears it as a 400
		// on the mapping rather than as a 500 on the first document.
		return errors.New("a field name is non-empty and holds no NUL byte")
	}
	if name == textField {
		return fmt.Errorf("%q is engine.Document's own text and is not a mapped field: it is always analysed "+
			"and it can hold nothing else", textField)
	}
	if !slices.Contains(supportedTypes, p.Type) {
		return fmt.Errorf("type %q is not one this server can honour (%s): a type accepted and then treated as "+
			"text would make a range query over it match nothing with nothing to report",
			p.Type, strings.Join(supportedTypes, ", "))
	}
	if p.Type == typeKNNVector {
		if p.Dimension <= 0 {
			return fmt.Errorf("a knn_vector needs a positive dimension; the index refuses a vector of a "+
				"different width than the corpus (engine.ErrDimMismatch), so the width is declared rather "+
				"than inferred from whichever document arrives first, got %d", p.Dimension)
		}
	} else if p.Dimension != 0 {
		return fmt.Errorf("dimension belongs to knn_vector and %q is %q", name, p.Type)
	}
	if p.Recency && p.Type != typeDate {
		return fmt.Errorf("recency belongs to a %s field and %q is %q: engine.Document.Time is a time, and a "+
			"keyword bound to it would decay from a number that is not one", typeDate, name, p.Type)
	}
	if p.Links && p.Type != typeKeyword {
		return fmt.Errorf("links belongs to a %s field and %q is %q: a link is another document's id, and an "+
			"analysed field would hold whatever the tokenizer made of that id — edges named by terms, "+
			"pointing at nothing", typeKeyword, name, p.Type)
	}
	return nil
}

// ---------------------------------------------------------------- index time

// indexTerm renders one JSON value as the text weft will tokenize for a field.
//
// This is half of the contract query.EncodeInt names: *the caller indexes it and
// the caller queries it*. rangeBound below is the other half, and the two are in
// one file so a change to either is read next to the other.
func indexTerm(field, kind string, raw json.RawMessage) (text string, indexed bool, err error) {
	switch kind {
	case typeInteger, typeLong:
		n, err := jsonInt(raw)
		if err != nil {
			return "", false, fmt.Errorf("%q is mapped as %q and this value is not: %w", field, kind, err)
		}
		return query.EncodeInt(n), true, nil
	case typeDate:
		t, err := jsonTime(raw)
		if err != nil {
			return "", false, fmt.Errorf("%q is mapped as a date and this value is not: %w", field, err)
		}
		return query.EncodeTime(t), true, nil
	default:
		// text, keyword and every undeclared field: the literal, as before this
		// milestone. keyword differs from text at query time and not here — see
		// termScorers.
		s, ok := scalarText(raw)
		return s, ok, nil
	}
}

// jsonInt reads a JSON number that is an integer, and refuses one that is not.
//
// A float truncated to an integer would index 3.7 as "3" and then not be found by
// a range whose bounds the client wrote around 3.7. Refusing is the narrower
// answer and the one the client can act on.
func jsonInt(raw json.RawMessage) (int64, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, fmt.Errorf("expected a number, got %s", raw)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f < math.MinInt64 || f > math.MaxInt64 {
		return 0, fmt.Errorf("expected a whole number in int64 range, got %s", raw)
	}
	return int64(f), nil
}

// dateFormats is what a date field accepts, and the list is closed.
//
// OpenSearch's default is `strict_date_optional_time||epoch_millis`, and these
// are the shapes of it a client actually sends. A format this server cannot read
// is a 400 rather than a zero time: a document dated to the year 1 sorts below
// every range a client will ever write and reports nothing about why.
var dateFormats = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

// jsonTime reads a date as a string in one of dateFormats or as epoch
// milliseconds.
func jsonTime(raw json.RawMessage) (time.Time, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		for _, layout := range dateFormats {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("expected one of %s or epoch milliseconds, got %q",
			strings.Join(dateFormats, ", "), s)
	}
	ms, err := jsonInt(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected a date string or epoch milliseconds, got %s", raw)
	}
	// UnixMilli rather than Unix: OpenSearch's epoch_millis is the one number a
	// client sends unlabelled, and reading it as seconds would date every
	// document to the 1970s without failing.
	return time.UnixMilli(ms).UTC(), nil
}

// jsonVector reads a knn_vector value and checks its width against the
// declaration.
//
// Checked here rather than left to engine.Add, because engine refuses a mismatched
// width for the whole write and this server would then be reporting one client's
// malformed vector as a server error to the next one.
func jsonVector(field string, dim int, raw json.RawMessage) ([]float32, error) {
	var v []float32
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%q is mapped as a knn_vector and this value is not an array of numbers: %w", field, err)
	}
	if len(v) != dim {
		return nil, fmt.Errorf("%q is mapped with dimension %d and this vector has %d: the index refuses a "+
			"width the corpus does not have (engine.ErrDimMismatch)", field, dim, len(v))
	}
	for i, f := range v {
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return nil, fmt.Errorf("%q has a non-finite component at index %d: cosine over it is not a number "+
				"and every comparison with it is false (engine.ErrNonFiniteVector)", field, i)
		}
	}
	return v, nil
}

// ---------------------------------------------------------------- query time

// rangeBound encodes one side of a range clause the way indexTerm encoded the
// documents.
//
// exclusive is what OpenSearch spells `gt` and `lt`. weft's query.Range is
// inclusive on both sides and there is no exclusive form, so an exclusive bound
// on a number or a date becomes the next representable value — the one step that
// is exact rather than approximate. On a text or keyword field there is no next
// term to step to, and this reports that rather than quietly widening the range
// by however many documents sit on the boundary.
func rangeBound(field, kind string, raw json.RawMessage, exclusive, upper bool) (string, error) {
	switch kind {
	case typeInteger, typeLong:
		n, err := jsonInt(raw)
		if err != nil {
			return "", fmt.Errorf("%q is mapped as %q and this bound is not: %w", field, kind, err)
		}
		if exclusive {
			if upper {
				if n == math.MinInt64 {
					return "", fmt.Errorf("%q has no value below %d", field, n)
				}
				n--
			} else {
				if n == math.MaxInt64 {
					return "", fmt.Errorf("%q has no value above %d", field, n)
				}
				n++
			}
		}
		return query.EncodeInt(n), nil
	case typeDate:
		t, err := jsonTime(raw)
		if err != nil {
			return "", fmt.Errorf("%q is mapped as a date and this bound is not: %w", field, err)
		}
		if exclusive {
			// One nanosecond, because that is what query.EncodeTime resolves.
			if upper {
				t = t.Add(-time.Nanosecond)
			} else {
				t = t.Add(time.Nanosecond)
			}
		}
		return query.EncodeTime(t), nil
	default:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			// A number against an unmapped field is the mistake this whole file
			// exists to catch: "42" sorts before "7" and the range is nonsense.
			var n float64
			if json.Unmarshal(raw, &n) == nil {
				return "", fmt.Errorf("%q is not mapped as a number or a date, so its terms are compared as "+
					"bytes and a numeric bound over them is nonsense — \"42\" sorts before \"7\". Map the "+
					"field as %s, %s or %s and re-index", field, typeInteger, typeLong, typeDate)
			}
			return "", fmt.Errorf("a range bound on %q is a string, got %s", field, raw)
		}
		if exclusive {
			return "", fmt.Errorf("gt and lt on %q need a field mapped as %s, %s or %s: its terms are compared "+
				"as bytes and there is no next byte string to step to, so an exclusive bound could only be "+
				"honoured by widening the range. Use gte or lte",
				field, typeInteger, typeLong, typeDate)
		}
		return s, nil
	}
}

// termLiteral escapes a value so query.Glob matches it as itself.
//
// Glob's syntax is path.Match's, where `*`, `?`, `[` and `\` mean something. A
// `term` query means none of them, so they are escaped rather than passed
// through: a client searching for the tag `a[b` should get a hit and not a
// malformed-pattern error.
func termLiteral(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '*' || r == '?' || r == '[' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// termScorers turns one literal value in one field into the streams that find it.
//
// The value goes through the index's own tokenizer, which is D-023's rule and not
// a choice: a query tokenized differently from the documents finds nothing and has
// nothing to report. That is also the whole of what `keyword` means here — see
// docs/LIMITATIONS.md, because one index has one tokenizer (there is no
// SetTokenizer and no per-field analyser) and a keyword holding two tokens is
// therefore found as a conjunction of them rather than as one indivisible term.
//
// Zero tokens is an error rather than an empty result. A value that survives
// tokenization as nothing cannot match any document, and answering 200 with no
// hits for it is exactly the silent nothing this package refuses.
func termScorers(ix *engine.Index, field, value string) ([]engine.Scorer, *apiError) {
	tokens := ix.Tokenize(value)
	if len(tokens) == 0 {
		return nil, badRequest("parsing_exception",
			"the value %q holds no term this index could have indexed, so no document can match it; "+
				"a term query is not a way to ask for nothing", value)
	}
	scorers := make([]engine.Scorer, 0, len(tokens))
	for _, tok := range tokens {
		scorers = append(scorers, query.Glob(ix, field, termLiteral(tok)))
	}
	return scorers, nil
}

// vocabularyFits reports whether a pattern's literal head names no more terms
// than query.MaxTerms.
//
// Glob stops the vocabulary scan at MaxTerms and cannot tell the caller it did.
// That silence is fine inside a Go program, where the caller chose the pattern;
// over HTTP it is a truncated answer presented as a complete one. Asking for one
// term more than the cap is what makes the truncation visible, and it costs one
// extra vocabulary entry.
func vocabularyFits(ix *engine.Index, field, pattern string) *apiError {
	head := literalHead(pattern)
	if len(ix.Terms(engine.FieldTerm(field, "")+head, query.MaxTerms+1)) > query.MaxTerms {
		return badRequest("too_many_clauses",
			"the pattern %q begins with %q, which names more than %d terms in this index; the scan is capped "+
				"there, so answering would mean returning part of the match as though it were all of it. "+
				"Give more of the term before the wildcard",
			pattern, head, query.MaxTerms)
	}
	return nil
}

// literalHead is the part of a pattern before its first metacharacter, which
// every term it can match must start with. query.literalPrefix is the same rule
// on the other side of the package boundary; it is unexported there and this is
// four lines rather than a change to pkg/.
func literalHead(pattern string) string {
	if i := strings.IndexAny(pattern, `*?[\`); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// parseFuzziness reads OpenSearch's fuzziness, which is a number or "AUTO".
//
// **The distance is in bytes**, which query.Fuzzy says of itself and this repeats
// because it is the difference a Korean or Japanese client will see first: one
// mistyped Hangul syllable is three byte edits, so it is past MaxEditDistance
// before it is one character wrong. PRD section 4 registers that as published
// rather than fixed.
func parseFuzziness(raw json.RawMessage, term string) (int, *apiError) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.EqualFold(s, "AUTO") {
			// OpenSearch's AUTO, over bytes for the reason above.
			switch n := len(term); {
			case n < 3:
				return 0, nil
			case n < 6:
				return 1, nil
			default:
				return query.MaxEditDistance, nil
			}
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, badRequest("parsing_exception",
				"fuzziness is a whole number in [0, %d] or \"AUTO\", got %q", query.MaxEditDistance, s)
		}
		return checkedDistance(n)
	}
	n, err := jsonInt(raw)
	if err != nil {
		return 0, badRequest("parsing_exception",
			"fuzziness is a whole number in [0, %d] or \"AUTO\", got %s", query.MaxEditDistance, raw)
	}
	return checkedDistance(int(n))
}

func checkedDistance(n int) (int, *apiError) {
	if n < 0 || n > query.MaxEditDistance {
		return 0, badRequest("parsing_exception",
			"fuzziness %d is outside [0, %d]: past two edits the candidate set stops being the word you meant "+
				"and becomes words of about that length", n, query.MaxEditDistance)
	}
	return n, nil
}
