// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/skyoo2003/weft/pkg/engine"
)

var (
	// ErrNoSuchIndex is a named index that is not open.
	ErrNoSuchIndex = errors.New("opensearch: no such index")

	// ErrIndexExists is a create over a name already in use.
	ErrIndexExists = errors.New("opensearch: index already exists")

	// ErrBadIndexName is a name this server will not route to. See validName.
	ErrBadIndexName = errors.New("opensearch: index name is empty, uppercase, leading-underscore, or holds a path separator")

	// ErrClosed is a write submitted after shutdown. Returned rather than
	// dropped: a client that got 200 for a write the server never performed has
	// no way to find out.
	ErrClosed = errors.New("opensearch: the index is closed")
)

// Registry is every open index, keyed by name, plus the directory they live in.
//
// One directory under root is one index. There is no cluster and no shard map:
// docs/LIMITATIONS.md rules distribution out, and inventing a placeholder for it
// here would be a second source of truth about a thing that does not exist.
type Registry struct {
	mu   sync.RWMutex
	root string
	idx  map[string]*Index
}

// OpenRegistry opens every index directly under root.
//
// A directory that will not open is a startup failure, not a skip. A server that
// came up without one of its indexes answers 404 for it, and a 404 is what a
// client also gets for an index that was never created — so skipping turns a
// disk problem into a silently wrong answer, which is the failure this package
// refuses everywhere else.
func OpenRegistry(root string) (*Registry, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", root, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}

	r := &Registry{root: root, idx: map[string]*Index{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		x, err := openIndex(root, e.Name())
		if err != nil {
			// Everything opened so far is released, so a failed startup leaves no
			// mappings behind.
			r.Close() //nolint:errcheck // the error being returned is the one that matters
			return nil, err
		}
		r.idx[e.Name()] = x
	}
	return r, nil
}

// Names is every open index, sorted, so a listing is stable between calls.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.idx))
	for name := range r.idx {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Get returns an open index by name.
func (r *Registry) Get(name string) (*Index, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	x, ok := r.idx[name]
	return x, ok
}

// Create makes an empty index and commits it, so the directory it leaves behind
// is one OpenRegistry can reopen.
//
// The commit is what makes an empty index durable at all: engine.Open reads a
// manifest, and a bare directory has none. Committing here rather than deferring
// to the first write means a created-then-restarted server still has the index
// the client was told it had.
func (r *Registry) Create(name string) (*Index, error) {
	if !validName(name) {
		return nil, fmt.Errorf("%q: %w", name, ErrBadIndexName)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.idx[name]; exists {
		return nil, fmt.Errorf("%q: %w", name, ErrIndexExists)
	}

	dir := filepath.Join(r.root, name)
	// validName above rejects a separator, a leading dot and the two relative
	// path elements, so name cannot escape r.root. That guard is the security
	// boundary and TestCreateRefusesANameThatWouldCollideWithARoute is what
	// holds it; the analyser cannot see across the call.
	if err := os.Mkdir(dir, 0o700); err != nil { //nolint:gosec // validName rejects every path element that could escape r.root
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	ix := engine.New()
	if err := ix.Commit(context.Background(), dir); err != nil {
		os.RemoveAll(dir) //nolint:errcheck,gosec // same guard as the Mkdir above; the commit error is the one to report
		return nil, fmt.Errorf("commit the empty index %q: %w", name, err)
	}

	x := newIndex(name, dir, ix, NewSource(), NewMapping())
	r.idx[name] = x
	return x, nil
}

// Drop closes an index and removes its directory.
//
// engine.Scrub is not this: it verifies a directory rather than removing one.
// There is no reclaiming half of a delete anywhere in weft — docs/LIMITATIONS.md
// says a full re-index is the only compaction — so dropping an index is exactly
// removing the directory.
func (r *Registry) Drop(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	x, ok := r.idx[name]
	if !ok {
		return fmt.Errorf("%q: %w", name, ErrNoSuchIndex)
	}
	delete(r.idx, name)
	if err := x.close(); err != nil {
		return err
	}
	if err := os.RemoveAll(x.dir); err != nil {
		return fmt.Errorf("remove %s: %w", x.dir, err)
	}
	return nil
}

// Close stops every write goroutine and releases every mapping.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var first error
	for name, x := range r.idx {
		if err := x.close(); err != nil && first == nil {
			first = fmt.Errorf("close %q: %w", name, err)
		}
		delete(r.idx, name)
	}
	return first
}

// validName is the subset of OpenSearch's index-name rules this server needs.
//
// A leading underscore is the one that is structural rather than cosmetic:
// /_bulk and /_cluster are routes, so an index called `_bulk` would make one path
// mean two things. The rest — no uppercase, no separator, not a relative path
// element — keep a name usable as a directory on every platform, which is what a
// name is here.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") {
		return false
	}
	if name != strings.ToLower(name) {
		return false
	}
	return !strings.ContainsAny(name, `/\:*?"<>| `)
}

// op is one unit of work for an index's writer goroutine.
//
// A function rather than a verb enum: every mutation is "hold the writer, do
// this, report", and a variant per verb would be three copies of that with the
// interesting line in the middle. ponytail: one goroutine per index, unbuffered.
// If write throughput becomes the complaint, the lever is batching inside this
// loop — a second writer buys nothing, because engine.Index.wmu already
// serializes mutators against each other.
type op struct {
	do   func() error
	done chan error
}

// Index is one weft index plus the two things the library does not hold for it:
// the JSON bodies, and a single writer.
type Index struct {
	name string
	dir  string
	ix   *engine.Index
	src  *Source
	m    *Mapping

	ops    chan op
	closed chan struct{}
	wg     sync.WaitGroup
}

func newIndex(name, dir string, ix *engine.Index, src *Source, m *Mapping) *Index {
	x := &Index{
		name:   name,
		dir:    dir,
		ix:     ix,
		src:    src,
		m:      m,
		ops:    make(chan op),
		closed: make(chan struct{}),
	}
	x.wg.Add(1)
	go x.serve()
	return x
}

func openIndex(root, name string) (*Index, error) {
	dir := filepath.Join(root, name)
	ix, err := engine.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open the index in %s: %w", dir, err)
	}
	live, _ := ix.Stats()
	src, err := LoadSource(dir, live)
	if err != nil {
		ix.Close() //nolint:errcheck // the source error is the one to report
		return nil, err
	}
	// After the source, because a mapping that will not load is the same class of
	// startup failure and reporting the first one found keeps the two messages
	// from racing. A directory written before milestone 25 has no mapping file
	// and loads as no declared fields, which is what it was.
	m, err := LoadMapping(dir)
	if err != nil {
		ix.Close() //nolint:errcheck // the mapping error is the one to report
		return nil, err
	}
	return newIndex(name, dir, ix, src, m), nil
}

// serve runs every mutation for this index, one at a time.
//
// It watches closed rather than ranging over a closed ops channel. Closing ops
// would be the shorter shutdown and it is wrong: a send on a closed channel
// panics even inside a select, so a write racing shutdown would take the process
// down instead of getting ErrClosed. An op already received always completes —
// an unbuffered send only returns once this loop holds it — so shutdown never
// strands a caller waiting on done.
func (x *Index) serve() {
	defer x.wg.Done()
	for {
		select {
		case o := <-x.ops:
			o.done <- o.do()
		case <-x.closed:
			return
		}
	}
}

// write submits fn to the writer goroutine and waits for it.
func (x *Index) write(fn func() error) error {
	o := op{do: fn, done: make(chan error, 1)}
	select {
	case x.ops <- o:
		return <-o.done
	case <-x.closed:
		return fmt.Errorf("%q: %w", x.name, ErrClosed)
	}
}

// Engine is the index the scorers read. Reads do not go through the writer
// goroutine: engine takes mu in read mode, so they already run in parallel with
// each other and with all but the swap at the end of a commit.
func (x *Index) Engine() *engine.Index { return x.ix }

// Source is the JSON body store.
func (x *Index) Source() *Source { return x.src }

// Mapping is what this server knows about a field that the index does not.
func (x *Index) Mapping() *Mapping { return x.m }

// Name is the index name a response echoes back as _index.
func (x *Index) Name() string { return x.name }

// Remap adds field declarations and publishes them.
//
// On the writer goroutine, and that is not decoration: a mapping decides what
// term a value becomes, so a declaration landing between the read of the mapping
// and the write of a document would index that document by neither rule. Nothing
// else in this package can put those two on the same lock.
//
// Published before the call returns, rather than at the next commit. A client
// that was told its mapping was accepted and then lost it to a crash would index
// its next thousand documents by the wrong rule.
func (x *Index) Remap(doc mappingDoc) error {
	return x.write(func() error {
		if err := x.m.merge(doc); err != nil {
			return err
		}
		return x.m.save(x.dir)
	})
}

// Apply runs a whole batch of actions under one hold of the writer.
//
// One hold, not one per action, which is the point of _bulk here: docs/D-017
// prices a commit at eleven seconds for twenty thousand documents, and a batch
// that reacquired the writer between items would interleave with every other
// client's writes for the whole of that.
//
// Returns one error per action, in order. A bulk request reports per-item status
// and does not fail as a unit — one malformed document out of a thousand is the
// client's to fix and not a reason to refuse the other nine hundred and
// ninety-nine.
func (x *Index) Apply(actions []*bulkAction) []error {
	errs := make([]error, len(actions))
	err := x.write(func() error {
		for i, a := range actions {
			errs[i] = a.apply(x)
		}
		return nil
	})
	if err != nil {
		// The batch never ran — the index is closed. Every action gets that same
		// answer rather than a nil that would read as success.
		for i := range errs {
			errs[i] = err
		}
	}
	return errs
}

// Put indexes a document and stores its body, reporting whether the document is
// new.
//
// Add and Update rather than Add alone: engine.Add refuses a duplicate key with
// ErrDuplicateKey, and PUT /{index}/_doc/{id} is both create and replace. The
// two are told apart by Resolve before the write, which is safe because this runs
// on the writer goroutine and nothing else mutates the index.
func (x *Index) Put(d engine.Document, body json.RawMessage) (created bool, err error) {
	err = x.write(func() error {
		created, err = x.putLocked(d, body)
		return err
	})
	return created, err
}

// putLocked is Put's body, and it runs on the writer goroutine.
//
// Split out so _bulk can do many of these under one hold of the writer without
// either path growing its own copy of the create-or-replace rule.
func (x *Index) putLocked(d engine.Document, body json.RawMessage) (created bool, err error) {
	_, exists := x.ix.Resolve(d.Key)
	created = !exists
	if exists {
		if _, err := x.ix.Update(d); err != nil {
			return false, fmt.Errorf("update %q in %q: %w", d.Key, x.name, err)
		}
	} else if _, err := x.ix.Add(d); err != nil {
		return false, fmt.Errorf("index %q in %q: %w", d.Key, x.name, err)
	}
	x.src.Put(d.Key, body)
	return created, nil
}

// DeleteDoc removes a document from the index and its body from the store,
// reporting whether it was there.
func (x *Index) DeleteDoc(id string) (found bool, err error) {
	err = x.write(func() error {
		found = x.deleteLocked(id)
		return nil
	})
	return found, err
}

// deleteLocked is DeleteDoc's body, on the writer goroutine.
func (x *Index) deleteLocked(id string) bool {
	found := x.ix.Delete(id)
	x.src.Delete(id)
	return found
}

// Commit makes everything written so far durable: the segments, then the bodies.
//
// Segments first. If the process dies between the two, what survives is a store
// behind the index rather than ahead of it — and LoadSource refuses both, with
// the numbers the other way round. A refusal either way is the point; the order
// only decides which number is larger in the message.
func (x *Index) Commit(ctx context.Context) error {
	return x.write(func() error {
		if err := x.ix.Commit(ctx, x.dir); err != nil {
			return fmt.Errorf("commit %q: %w", x.name, err)
		}
		if err := x.src.Save(x.dir); err != nil {
			return fmt.Errorf("save the _source store for %q: %w", x.name, err)
		}
		return nil
	})
}

// Merge rewrites the segment set and commits the result.
//
// On the writer goroutine, like every other mutation: Merge replaces the
// segments under the index, and a commit racing it would be writing a manifest
// for a set that is being taken apart.
//
// The commit is not optional. engine.Merge changes what is in memory, and a
// merged index never committed leaves the directory holding exactly the segments
// the merge existed to replace — so a client would be told the merge happened
// and find the old layout after a restart.
func (x *Index) Merge(ctx context.Context) error {
	return x.write(func() error {
		if err := x.ix.Merge(); err != nil {
			return fmt.Errorf("merge %q: %w", x.name, err)
		}
		if err := x.ix.Commit(ctx, x.dir); err != nil {
			return fmt.Errorf("commit the merge of %q: %w", x.name, err)
		}
		return nil
	})
}

// Scrub verifies what is committed in this index's directory.
//
// On the writer goroutine as well, for a reason the read-only signature hides:
// engine.Scrub walks the files a commit replaces, so one running alongside a
// commit reads half of each generation and reports the damage that reading
// caused. It checks the committed generation and nothing else — writes since the
// last commit are not on disk to be checked.
func (x *Index) Scrub() error {
	return x.write(func() error {
		if err := engine.Scrub(x.dir); err != nil {
			return fmt.Errorf("scrub %q: %w", x.name, err)
		}
		return nil
	})
}

// close stops the writer and releases the index's mappings.
func (x *Index) close() error {
	select {
	case <-x.closed:
		return nil // already closed
	default:
	}
	close(x.closed)
	x.wg.Wait()
	if err := x.ix.Close(); err != nil {
		return fmt.Errorf("close %q: %w", x.name, err)
	}
	return nil
}
