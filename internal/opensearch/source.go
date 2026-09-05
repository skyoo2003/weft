// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// sourceFile is the store's name inside an index directory. It sits beside the
// segments rather than inside them because engine.Document has no field for
// arbitrary JSON and should not grow one: putting the raw body in Document.Text
// or a Field would tokenize it, and the vocabulary would then hold every key and
// brace the client ever sent.
const sourceFile = "_source.json"

// ErrSourceMismatch reports a store and an index that disagree about how many
// documents exist.
//
// The two are written by the same request and published by the same rename, so
// disagreement means one of them was lost — an index committed by a program that
// did not keep a store, or a store left behind by one that did. Neither is
// recoverable here, and neither is safe to continue from: search would find a
// document the response cannot show, which is a wrong answer rather than a
// narrow one.
var ErrSourceMismatch = errors.New("opensearch: the _source store and the index hold different numbers of documents")

// Source is the JSON body of every document, keyed by engine.Document.Key.
//
// It is the caller-held side store docs/LIMITATIONS.md prescribes for anything a
// weft index does not itself hold, and it is the server's, not the library's —
// see D-025.
//
// ponytail: the whole map is marshalled on every Save. An offset index is the
// upgrade path, and the trigger for it is Save showing up in a commit's wall
// clock rather than a corpus reaching any particular size.
type Source struct {
	mu   sync.RWMutex
	docs map[string]json.RawMessage
}

// NewSource returns an empty store.
func NewSource() *Source {
	return &Source{docs: map[string]json.RawMessage{}}
}

// LoadSource reads the store in dir and checks it against the index's live
// document count.
//
// A directory with no store is an empty store, which is correct for an index
// that has never been written to and wrong for any other — hence live, which is
// engine.Index.Stats' first return and not Len: Len counts tombstones and would
// make every deleted document look like a lost body.
func LoadSource(dir string, live int) (*Source, error) {
	s := NewSource()
	path := filepath.Join(dir, sourceFile)

	// The path is filepath.Join of a directory this process chose and a
	// constant filename; no part of it comes from a request. Registry.validName
	// is what keeps the directory half of that true, and it is tested.
	raw, err := os.ReadFile(path) //nolint:gosec // path is a constant name under a directory this process owns
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Left empty. The count check below is what decides whether that is the
		// new-index case or a store that went missing.
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	default:
		if err := json.Unmarshal(raw, &s.docs); err != nil {
			return nil, fmt.Errorf("%s did not decode, so the documents it holds cannot be served and "+
				"treating it as empty would hide that: repair or remove it and re-index: %w", path, err)
		}
	}

	if len(s.docs) != live {
		return nil, fmt.Errorf("%s holds %d documents and the index holds %d live, so a search would "+
			"find documents this store cannot show: re-index, or remove the directory and start over: %w",
			path, len(s.docs), live, ErrSourceMismatch)
	}
	return s, nil
}

// Get returns the stored body for a key.
func (s *Source) Get(key string) (json.RawMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, ok := s.docs[key]
	return raw, ok
}

// Put stores a body, replacing any previous one.
//
// The bytes are cloned. They arrive out of a request body this package decodes
// into a reused buffer, and a store aliasing them would serve whatever the next
// request left there.
func (s *Source) Put(key string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[key] = bytes.Clone(raw)
}

// Delete removes a body and reports whether it was there.
func (s *Source) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.docs[key]; !ok {
		return false
	}
	delete(s.docs, key)
	return true
}

// Len is how many bodies are stored.
func (s *Source) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.docs)
}

// Save writes the store into dir, atomically against process death.
//
// Temporary file then rename, which is what engine.Index.Commit does with a
// manifest and for the same reason: a reader either sees the whole previous
// store or the whole new one. Durability past that stops at fsync, exactly as
// docs/FORMAT.md section 6 says it does for the index this sits beside.
func (s *Source) Save(dir string) error {
	s.mu.RLock()
	raw, err := json.Marshal(s.docs)
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("encode the _source store: %w", err)
	}

	// In dir rather than in TMPDIR, because rename is only atomic within one
	// filesystem and TMPDIR is routinely on another.
	tmp, err := os.CreateTemp(dir, sourceFile+".*")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	// Removed on every failure path below. A leftover would be a file the next
	// LoadSource has to know to ignore, which is a second rule where there
	// should be none.
	defer os.Remove(tmp.Name()) //nolint:errcheck // best effort; the rename below is what matters

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close() //nolint:errcheck // the write already failed
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck // the sync already failed
		return fmt.Errorf("sync %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, sourceFile)); err != nil { //nolint:gosec // same: a constant name under an owned directory
		return fmt.Errorf("publish %s: %w", sourceFile, err)
	}
	return nil
}
