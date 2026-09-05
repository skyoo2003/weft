// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/skyoo2003/weft/pkg/engine"
)

func doc(key, text string) engine.Document {
	return engine.Document{Key: key, Text: text}
}

func mustCreate(t *testing.T, r *Registry, name string) *Index {
	t.Helper()
	x, err := r.Create(name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return x
}

func TestRegistryReopensWhatWasCommitted(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	x := mustCreate(t, r, "papers")
	if _, err := x.Put(doc("1", "reciprocal rank fusion"), json.RawMessage(`{"text":"reciprocal rank fusion"}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := x.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("OpenRegistry after Close: %v", err)
	}
	defer reopened.Close() //nolint:errcheck // the assertions below are what this test is for

	x2, ok := reopened.Get("papers")
	if !ok {
		t.Fatal(`Get("papers") after reopen: not found`)
	}
	if live, _ := x2.Engine().Stats(); live != 1 {
		t.Errorf("live documents = %d, want 1", live)
	}
	body, ok := x2.Source().Get("1")
	if !ok {
		t.Fatal(`_source for "1" after reopen: not found`)
	}
	if string(body) != `{"text":"reciprocal rank fusion"}` {
		t.Errorf("_source = %s, want the body that was indexed", body)
	}
}

// A directory under -data that will not open is a startup failure, not a warning.
// A half-started server answers 404 for an index that exists, and a 404 is
// indistinguishable from "never created" to every client — the same silent wrong
// answer this package refuses everywhere else.
func TestRegistryRefusesADirectoryItCannotOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "broken"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	r, err := OpenRegistry(dir)
	if err == nil {
		r.Close() //nolint:errcheck // the failure below is the point
		t.Fatal("OpenRegistry over a directory with no manifest: err = nil, want a failure")
	}
}

func TestCreateRefusesADuplicateName(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	defer r.Close() //nolint:errcheck // best effort at the end of a test

	mustCreate(t, r, "papers")
	if _, err := r.Create("papers"); !errors.Is(err, ErrIndexExists) {
		t.Fatalf("Create twice: err = %v, want ErrIndexExists", err)
	}
}

// An index whose name starts with an underscore would collide with the API's own
// paths — /_bulk and /_cluster are routes, not indexes. OpenSearch refuses the
// same names for the same reason.
func TestCreateRefusesANameThatWouldCollideWithARoute(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	defer r.Close() //nolint:errcheck // best effort at the end of a test

	for _, name := range []string{"_bulk", "", "papers/2024", "..", "Papers"} {
		if _, err := r.Create(name); !errors.Is(err, ErrBadIndexName) {
			t.Errorf("Create(%q): err = %v, want ErrBadIndexName", name, err)
		}
	}
}

func TestDropRemovesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	defer r.Close() //nolint:errcheck // best effort at the end of a test

	mustCreate(t, r, "papers")
	if err := r.Drop("papers"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if _, ok := r.Get("papers"); ok {
		t.Error("Get after Drop: found, want absent")
	}
	if _, err := os.Stat(filepath.Join(dir, "papers")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory survived Drop: stat err = %v, want ErrNotExist", err)
	}
	if err := r.Drop("papers"); !errors.Is(err, ErrNoSuchIndex) {
		t.Errorf("Drop twice: err = %v, want ErrNoSuchIndex", err)
	}
}

// engine.Index.Add blocks for the whole of a commit (docs/LIMITATIONS.md,
// D-017), so an HTTP handler calling it directly hands one slow commit every
// request that arrives during it. One goroutine per index is what keeps that
// queue inside the server where it can be seen. This asserts the property that
// buys — every write lands exactly once under -race — rather than the timing,
// which belongs to engine and is already measured at 61 ms (FINDINGS milestone 9).
func TestConcurrentWritesAllLandExactlyOnce(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	defer r.Close() //nolint:errcheck // best effort at the end of a test
	x := mustCreate(t, r, "papers")

	const writers = 32
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := string(rune('a' + i%26))
			if i >= 26 {
				key += "x"
			}
			_, errs[i] = x.Put(doc(key, "term "+key), json.RawMessage(`{}`))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if live, _ := x.Engine().Stats(); live != writers {
		t.Errorf("live documents = %d, want %d", live, writers)
	}
	if n := x.Source().Len(); n != writers {
		t.Errorf("_source holds %d documents, want %d", n, writers)
	}
}

// A write submitted after shutdown has to be refused. Dropping it silently would
// acknowledge to a client something the server never did.
func TestAWriteAfterCloseIsRefused(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	x := mustCreate(t, r, "papers")
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := x.Put(doc("1", "late"), json.RawMessage(`{}`)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close: err = %v, want ErrClosed", err)
	}
}

// Update is the same route as create, and the second write must replace rather
// than add — engine.Add returns ErrDuplicateKey, so the server has to choose
// Update, and a document counted twice is the failure this catches.
func TestPuttingTheSameIDTwiceReplaces(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	defer r.Close() //nolint:errcheck // best effort at the end of a test
	x := mustCreate(t, r, "papers")

	created, err := x.Put(doc("1", "first"), json.RawMessage(`{"v":1}`))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !created {
		t.Error("first Put reported created = false, want true")
	}
	created, err = x.Put(doc("1", "second"), json.RawMessage(`{"v":2}`))
	if err != nil {
		t.Fatalf("Put again: %v", err)
	}
	if created {
		t.Error("second Put reported created = true, want false")
	}

	if live, _ := x.Engine().Stats(); live != 1 {
		t.Errorf("live documents = %d, want 1", live)
	}
	body, _ := x.Source().Get("1")
	if string(body) != `{"v":2}` {
		t.Errorf("_source = %s, want the second body", body)
	}
}

func TestDeleteDocRemovesFromBothSides(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	defer r.Close() //nolint:errcheck // best effort at the end of a test
	x := mustCreate(t, r, "papers")

	if _, err := x.Put(doc("1", "gone soon"), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	found, err := x.DeleteDoc("1")
	if err != nil {
		t.Fatalf("DeleteDoc: %v", err)
	}
	if !found {
		t.Error("DeleteDoc reported found = false, want true")
	}
	if live, _ := x.Engine().Stats(); live != 0 {
		t.Errorf("live documents = %d, want 0", live)
	}
	if _, ok := x.Source().Get("1"); ok {
		t.Error("_source still holds the deleted document")
	}

	found, err = x.DeleteDoc("1")
	if err != nil {
		t.Fatalf("DeleteDoc twice: %v", err)
	}
	if found {
		t.Error("DeleteDoc twice reported found = true, want false")
	}
}
