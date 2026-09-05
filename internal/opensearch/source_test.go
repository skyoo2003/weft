// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The _source store is the one piece of OpenSearch state weft's index cannot
// hold: engine.Document has Key, Text, Vector, Links, Time and Fields, and none
// of those is "the JSON the client sent". docs/LIMITATIONS.md already prescribes
// the shape for anything in that position — a caller-held store, reloaded keyed
// by Document.Key — and pkg/engine's own TestASideStoreSurvivesCommitAndOpen is
// the precedent these tests mirror.

func TestSourceSurvivesSaveAndLoad(t *testing.T) {
	dir := t.TempDir()

	s := NewSource()
	s.Put("a", json.RawMessage(`{"title":"first"}`))
	s.Put("b", json.RawMessage(`{"title":"second"}`))
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := LoadSource(dir, 2)
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	got, ok := reopened.Get("b")
	if !ok {
		t.Fatal(`Get("b") after reload: not found`)
	}
	if string(got) != `{"title":"second"}` {
		t.Errorf(`Get("b") = %s, want {"title":"second"}`, got)
	}
	if reopened.Len() != 2 {
		t.Errorf("Len() = %d, want 2", reopened.Len())
	}
}

// The failure this refuses is a directory whose index and _source have drifted
// apart: search finds the document and the response cannot show it. Silence
// there is a wrong answer rather than a narrow one, which is the rule
// pkg/query's documentation states for queries and D-023's guard states for a
// tokenizer.
func TestSourceRefusesACountItCannotExplain(t *testing.T) {
	dir := t.TempDir()

	s := NewSource()
	s.Put("a", json.RawMessage(`{}`))
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err := LoadSource(dir, 7)
	if !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("LoadSource with a live count of 7 over 1 stored document: err = %v, want ErrSourceMismatch", err)
	}
}

// A directory with no _source.json at all is the normal state of a brand new
// index, and only the normal state when the index is empty too.
func TestSourceLoadsAnEmptyDirectoryAsEmpty(t *testing.T) {
	s, err := LoadSource(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("LoadSource on an empty directory: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0", s.Len())
	}
}

func TestSourceDeleteRemovesTheDocument(t *testing.T) {
	s := NewSource()
	s.Put("a", json.RawMessage(`{}`))

	if !s.Delete("a") {
		t.Error(`Delete("a") = false, want true`)
	}
	if _, ok := s.Get("a"); ok {
		t.Error(`Get("a") after Delete: found, want absent`)
	}
	if s.Delete("a") {
		t.Error(`Delete("a") twice = true, want false`)
	}
}

// Save writes through a temporary file and renames, so a crash mid-write leaves
// the previous store rather than a truncated one. The leftover this checks for
// is the other half of that: a temp file that outlives the rename is a file the
// next Load has to know to ignore, which is a second rule where there should be
// none.
func TestSourceSaveLeavesOnlyTheStore(t *testing.T) {
	dir := t.TempDir()

	s := NewSource()
	s.Put("a", json.RawMessage(`{}`))
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Save(dir); err != nil {
		t.Fatalf("Save twice: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != sourceFile {
		t.Errorf("directory holds %v, want exactly [%s]", names, sourceFile)
	}
}

// A store written by a crashed process, or hand-edited, must not read back as an
// empty one — that is the count check failing open.
func TestSourceRefusesAStoreItCannotDecode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sourceFile), []byte(`{"a":`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := LoadSource(dir, 1); err == nil {
		t.Fatal("LoadSource over a truncated store: err = nil, want a decode error")
	}
}
