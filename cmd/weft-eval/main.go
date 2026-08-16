// SPDX-License-Identifier: Apache-2.0

// Command weft-eval measures ranking quality for milestone 4.
//
// It is the reproducibility half of the milestone: every number published in
// docs/EVAL.md has to come back out of one of these subcommands. See that
// document for the measurement design and the judgment rule.
//
//	weft-eval prepare    join the corpus to Semantic Scholar (slow, resumable)
//	weft-eval build      index the corpus and Commit it
//	weft-eval diagnose   graph signal degeneracy: hop-level candidate counts
//	weft-eval run        the frozen three-arm measurement
//	weft-eval sweep      sensitivity of the verdict to the frozen constants
//	weft-eval weights    the graph stream's fusion weight sweep
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/skyoo2003/weft/internal/eval"
	"github.com/skyoo2003/weft/pkg/engine"
)

// Default layout under -data. Everything here is gitignored and reproducible.
const (
	corpusFile   = "trec-covid/corpus.jsonl"
	queriesFile  = "trec-covid/queries.jsonl"
	qrelsFile    = "trec-covid/qrels/test.tsv"
	metadataFile = "metadata.csv"
	s2File       = "s2.jsonl"
	indexDir     = "index"

	// s2UnpinnedFile marks an s2.jsonl whose join ran against inputs the snapshot
	// table does not pin — that is, a `prepare -any-snapshot`.
	//
	// The flag says the numbers measured from it are not the ones docs/EVAL.md
	// publishes, and until now that claim died with the command that made it. The
	// cache it produced is an ordinary complete cache: a later plain `build` finds
	// full coverage, hashes the pinned corpus.jsonl, and records partial=false, so
	// `run` publishes vector and graph arms whose edges and vectors came from a
	// metadata release nobody verified. The marker is what carries the flag across
	// that gap.
	//
	// A file rather than a field, because the cache is append-only and cumulative:
	// the taint belongs to the whole of it, not to the records one invocation
	// appended. It is created, never removed by a later pinned run, for the same
	// reason — a pinned prepare resuming an unpinned one does not un-taint what is
	// already in the file. Deleting s2.jsonl is what starts over.
	s2UnpinnedFile = "s2.unpinned"

	// queryVecFile is written by internal/eval/testdata/gen_query_vectors.py. It is
	// optional; run reports its absence rather than quietly measuring a text-only
	// baseline.
	queryVecFile = "query-vectors.jsonl"

	// queryVecModel is what that script records in each record it writes: the SPECTER2
	// base plus the ad-hoc query adapter, which is the query-side counterpart of the
	// eval.S2Model the document vectors carry. Written here as a literal because the
	// Go side never loads a model — it only checks that the file says it used this one.
	queryVecModel = "allenai/specter2_base+allenai/specter2_adhoc_query"
)

// Subcommand names. Written once because two places have to agree on each: the
// switch in main, and the FlagSet the subcommand builds. The FlagSet name is what
// -h prints and what a leftover-argument error quotes back, so a drift between the
// two tells an operator about a command they did not run.
const (
	cmdPrepare  = "prepare"
	cmdBuild    = "build"
	cmdDiagnose = "diagnose"
	cmdRun      = "run"
	cmdSweep    = "sweep"
	cmdWeights  = "weights"
)

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
	}

	// Ctrl-C cancels rather than kills. prepare is a job measured in hours whose
	// value is entirely in its append-only output file; a signal that terminates
	// mid-write is the one way to corrupt it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case cmdPrepare:
		err = prepare(ctx, args)
	case cmdBuild:
		err = build(ctx, args)
	case cmdDiagnose:
		err = diagnose(ctx, args)
	case cmdRun:
		err = run(ctx, args)
	case cmdSweep:
		err = sweep(ctx, args)
	case cmdWeights:
		err = weights(ctx, args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "weft-eval: unknown subcommand %q\n", cmd)
		usage()
	}
	// Called rather than deferred: log.Fatal below exits through os.Exit, which
	// runs no deferred function, so a `defer stop()` here would be a handler that
	// only ever ran on the success path.
	stop()
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: weft-eval <prepare|build|diagnose|run|sweep|weights> [flags]

  prepare   join corpus ids to Semantic Scholar for citation edges and SPECTER
            vectors. Slow (hours, rate limited) and resumable — rerun to continue.
  build     index the corpus with links and vectors, then Commit it.
  diagnose  measure the graph scorer's hop-level candidate distribution.
  run       the frozen three-arm nDCG@10 measurement with bootstrap intervals.
  sweep     re-measure across RRF k and over-fetch to test the verdict's stability.
  weights   discount the graph stream in fusion and see whether that recovers it.
            Weights attach to stream position, not scorer kind, so fusion still
            does not know what it is holding.

Run any subcommand with -h for its flags. docs/EVAL.md is the measurement design.
`)
	os.Exit(2)
}

// dataFlags registers -data plus a subcommand's own flags and parses args.
//
// A leftover positional argument is refused rather than ignored, because flag stops
// parsing at the first non-flag argument and says nothing: `weft-eval prepare typo
// -limit=1` leaves -limit at its default, and the default here means unlimited. The
// typo therefore starts hours of rate-limited API requests, appending to the resumable
// cache, having silently discarded the flag that was meant to keep it small. Every
// subcommand takes its input from flags alone, so there is no argument this could be
// throwing away on purpose.
func dataFlags(name string, args []string, extra func(*flag.FlagSet)) (*string, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	dir := fs.String("data", ".eval-data", "directory holding the downloaded corpus and generated artifacts")
	if extra != nil {
		extra(fs)
	}
	_ = fs.Parse(args) // ExitOnError already handled a bad flag.
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%s takes no arguments, and %q is not a flag: parsing stopped there, "+
			"so every flag after it was ignored and left at its default", name, fs.Arg(0))
	}
	return dir, nil
}

// ---------------------------------------------------------------- prepare

func prepare(ctx context.Context, args []string) error {
	var apiKey string
	var limit int
	var anySnapshot bool
	data, flagErr := dataFlags(cmdPrepare, args, func(fs *flag.FlagSet) {
		fs.StringVar(&apiKey, "api-key", os.Getenv("S2_API_KEY"),
			"Semantic Scholar API key (optional; raises the rate limit)")
		fs.IntVar(&limit, "limit", 0,
			"stop after this many documents (0 = all); for smoke-testing the pipeline")
		snapshotFlag(fs, &anySnapshot)
	})
	if flagErr != nil {
		return flagErr
	}

	// Before the corpus is read and long before anything is appended. -limit is the
	// smoke-test flag, so the value a typo produces is the one that matters: 0 means
	// unlimited, and `limit > 0` therefore read -1 as unlimited too. A mistyped
	// `-limit=-1` then started fetching the whole corpus — hours of rate-limited
	// requests against a shared anonymous budget, appending to the resumable cache the
	// whole way — with nothing in the log saying the flag had been ignored.
	if limit < 0 {
		return fmt.Errorf("-limit=%d: the number of documents to fetch cannot be negative "+
			"(0 means all of them)", limit)
	}

	corpusPath := filepath.Join(*data, corpusFile)
	metaPath := filepath.Join(*data, metadataFile)
	outPath := filepath.Join(*data, s2File)

	// Before anything is written. This command's output is a cache that later commands
	// trust as a record of what was asked, so a truncated metadata file here becomes a
	// corpus recorded as permanently unjoinable — see the snapshot table.
	unpinnedPath := filepath.Join(*data, s2UnpinnedFile)
	if anySnapshot {
		// Written before the first append, so a run killed halfway still leaves the
		// cache described. See s2UnpinnedFile.
		if err := markUnpinned(unpinnedPath); err != nil {
			return err
		}
	} else {
		if err := verifySnapshot(*data, corpusFile, metadataFile); err != nil {
			return err
		}
		// A pinned run does not clear the mark: what an earlier -any-snapshot run
		// appended is still in the file this one is about to extend.
		if _, err := os.Stat(unpinnedPath); err == nil {
			log.Printf("WARNING: %s exists, so part of %s was joined against inputs the snapshot",
				unpinnedPath, outPath)
			log.Printf("WARNING: table does not pin. An index built from it is marked unpublishable.")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", unpinnedPath, err)
		}
	}

	// 1. Corpus ids. Reading these first means a missing or truncated corpus fails
	// before an hours-long fetch rather than during it.
	var uids []string
	inCorpus := map[string]bool{}
	if err := eval.ReadCorpus(corpusPath, func(d eval.CorpusDoc) error {
		uids = append(uids, d.ID)
		inCorpus[d.ID] = true
		return nil
	}); err != nil {
		return err
	}
	log.Printf("corpus: %d documents", len(uids))

	// 2. Resume. The output is append-only JSONL, so what is already in it is
	// exactly what does not need refetching. The corpus goes in because one of the
	// tallies is evidence about *this* corpus and the file may hold another one's —
	// see s2Cache.joined.
	cache, err := scanS2Cache(outPath, inCorpus)
	if err != nil {
		return err
	}
	if len(cache.keys) > 0 {
		log.Printf("resume: %d documents already in %s (%d joined to an identifier)",
			len(cache.keys), outPath, cache.joined)
	}

	// A hard kill can leave a half-written trailing record. Reading tolerates that,
	// but appending after it would splice the fragment into the next record and make
	// everything written from here on unreadable — silently, and only discovered by
	// the next resume, which would then refetch hours of work it already has. Drop
	// the fragment instead.
	if fi, err := os.Stat(outPath); err == nil && fi.Size() > cache.good {
		log.Printf("%s: dropping %d trailing bytes of an incomplete record", outPath, fi.Size()-cache.good)
		if err := os.Truncate(outPath, cache.good); err != nil {
			return fmt.Errorf("truncate %s: %w", outPath, err)
		}
	}

	want := make(map[string]bool, len(uids))
	for _, uid := range uids {
		if !cache.keys[uid] {
			want[uid] = true
		}
	}
	if len(want) == 0 {
		log.Printf("nothing to fetch; %s is complete", outPath)
		// Reported on the way out as well as at the end of a fetch. This is the run an
		// operator makes to confirm the cache is finished, and it is the only one that
		// sees every record — so it is the one place a model mismatch introduced by
		// some earlier interrupted run is guaranteed to surface.
		reportModels(cache.models)
		return nil
	}

	// 3. The join. ReadCORD19IDs checks the header before reading a single row, so
	// a release that cannot be joined at all fails immediately instead of after
	// scanning 1.6 GB.
	log.Printf("scanning %s for %d cord_uids...", metaPath, len(want))
	ids, tally, err := eval.ReadCORD19IDs(metaPath, want)
	// ReadCORD19IDs takes no context: it is one streaming pass over 1.6 GB, and a
	// Ctrl-C during it is absorbed by signal.NotifyContext rather than killing the
	// process, so the scan runs to EOF whatever the operator asked for. Checked
	// here, before the tombstones below, because those are the part of this command
	// that changes the cache — appending them under a cancellation would record
	// every unjoinable document as asked-and-answered on the way out, which is the
	// one thing an interrupted prepare must not leave behind.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil && !errors.Is(err, eval.ErrEmptyDataset) {
		return err
	}
	// "None of the remaining keys joined" is a state to record, not a failure. It is
	// what a resumed run sees once every joinable document is already cached and only
	// the unjoinable ones are left — the normal end of this command, reached through
	// the same error ReadCORD19IDs raises for a metadata file that matched nothing.
	// The tombstones below are what make it terminal rather than permanent: without
	// them the same keys come back on every rerun, each one rescanning 1.6 GB to be
	// told again that they cannot be joined. A header with no usable identifier
	// column at all is a different error and is still fatal.
	//
	// The two readings of that one error are only distinguishable from the cache. A
	// finished join left records carrying a CorpusId behind it; a metadata file that
	// belongs to another release, or one whose rows were all unreadable, leaves none.
	// Without this guard the second case is *also* terminal, and terminal here means
	// writing an asked-and-unjoinable record for every document in the corpus: build's
	// coverage gate then sees a complete cache, and the run publishes vector and graph
	// arms over an index with no vectors and no edges under their usual names. That is
	// the failure of docs/EVAL.md section 4.1 rebuilt out of the check meant to stop
	// it, so zero matches with no prior evidence of a join is refused.
	if errors.Is(err, eval.ErrEmptyDataset) && cache.joined == 0 {
		return fmt.Errorf("none of the %d cord_uids matched a row in %s, and no record in %s carries a "+
			"CorpusId, so nothing shows this metadata release can be joined to this corpus at all; "+
			"recording every document as unjoinable would let `weft-eval build` report vector and graph "+
			"arms over an index with neither. Check that %s is the release named in docs/DATASETS.md: %w",
			len(want), metaPath, outPath, metaPath, err)
	}
	log.Printf("join: %d of %d resolved to an identifier", len(ids), len(want))
	for _, k := range sortedKeys(tally) {
		log.Printf("  %-16s %d", k, tally[k])
	}

	// Fetched in corpus order rather than map order, so a partial run is a prefix
	// of the corpus and two partial runs are comparable.
	type target struct{ key, ref string }
	targets := make([]target, 0, len(ids))
	for _, uid := range uids {
		if id, ok := ids[uid]; ok && id.S2Ref != "" {
			targets = append(targets, target{uid, id.S2Ref})
		}
	}

	// Computed before -limit is applied: a document is unjoinable because the
	// metadata has no identifier for it, which has nothing to do with how many
	// documents this particular run was asked to fetch.
	joinable := make(map[string]bool, len(targets))
	for _, t := range targets {
		joinable[t.key] = true
	}
	var unjoinable []string
	for _, uid := range uids {
		if want[uid] && !joinable[uid] {
			unjoinable = append(unjoinable, uid)
		}
	}
	if len(unjoinable) > 0 {
		log.Printf("  %-16s %d (no usable identifier; recorded as asked-and-unjoinable so a "+
			"rerun does not rescan for them, and they stay in the corpus with no edges or vector)",
			"unresolved", len(unjoinable))
	}

	if limit > 0 && limit < len(targets) {
		targets = targets[:limit]
		log.Printf("limit: fetching only the first %d", limit)
	}

	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", outPath, err)
	}
	defer out.Close()
	enc := json.NewEncoder(out)

	// Written first, and written even though there is nothing to fetch for them. A
	// record with a key and nothing else says "this document was asked about and has
	// no Semantic Scholar side", which is exactly what build needs to know and what
	// the next resume needs in order to stop asking. It reads back as a hit with no
	// CorpusID, no refs and no vector, which is what an unjoinable document is.
	for _, uid := range unjoinable {
		if err := enc.Encode(eval.S2Record{Key: uid}); err != nil {
			return fmt.Errorf("write %s: %w", outPath, err)
		}
	}
	if len(unjoinable) > 0 {
		if err := out.Sync(); err != nil {
			return fmt.Errorf("sync %s: %w", outPath, err)
		}
	}
	if len(targets) == 0 {
		log.Printf("nothing left to fetch; %s is complete", outPath)
		reportModels(cache.models)
		return nil
	}

	client := eval.NewS2Client(apiKey, log.Printf)
	batches := (len(targets) + eval.S2BatchLimit - 1) / eval.S2BatchLimit
	if apiKey == "" {
		log.Printf("no API key: pacing at %s per request, %d batches to go (~%s)",
			client.Pace, batches, estimate(len(targets)))
	}

	// cache.models is added to, not replaced: this run's own batches join what earlier
	// runs already wrote, so the tally covers the whole file rather than the tail this
	// invocation happened to fetch.
	var fetched, missing, withVec, withRefs, rejected, edges int
	start := time.Now()
	for i := 0; i < len(targets); i += eval.S2BatchLimit {
		end := min(i+eval.S2BatchLimit, len(targets))
		batch := targets[i:end]

		refs := make([]string, len(batch))
		for j, t := range batch {
			refs[j] = t.ref
		}
		papers, err := client.Batch(ctx, refs)
		if err != nil {
			// What is on disk is valid and complete for the keys it holds, so this
			// is resumable. Saying so is the difference between the operator
			// rerunning and the operator starting over.
			return fmt.Errorf("batch at offset %d (%d already written to %s, rerun to resume): %w",
				i, fetched, outPath, err)
		}

		for j, p := range papers {
			rec := eval.S2Record{Key: batch[j].key}
			if p == nil {
				missing++
			} else {
				rec.CorpusID, rec.Refs, rec.Vector = p.CorpusID, p.Refs, p.Vector
				if len(p.Vector) > 0 {
					// Written to the record as well as counted, so the tally can be
					// rebuilt by a later run that did not fetch this batch.
					rec.Model = p.Model
					withVec++
					cache.models[p.Model]++
				}
				if p.VectorRejected {
					rejected++
				}
				if len(p.Refs) > 0 {
					withRefs++
					edges += len(p.Refs)
				}
			}
			// Written even when the paper was not found: the key is then known to
			// have been asked about, so a resumed run does not ask again.
			if err := enc.Encode(rec); err != nil {
				return fmt.Errorf("write %s: %w", outPath, err)
			}
			fetched++
		}
		if err := out.Sync(); err != nil {
			return fmt.Errorf("sync %s: %w", outPath, err)
		}
		log.Printf("%d/%d  missing=%d vec=%d refs=%d edges=%d  elapsed=%s",
			fetched, len(targets), missing, withVec, withRefs, edges,
			time.Since(start).Round(time.Second))
	}

	log.Printf("prepare done in %s", time.Since(start).Round(time.Second))
	log.Printf("  written    %d", fetched)
	log.Printf("  not found  %d", missing)
	log.Printf("  vectors    %d (%d rejected as unusable: non-finite or all zero)", withVec, rejected)
	reportModels(cache.models)
	log.Printf("  citing     %d documents, %d edges", withRefs, edges)
	return nil
}

// reportModels prints the embedding-model tally for every vector in the cache.
//
// Provenance, printed rather than assumed. Every published vector number is only as
// comparable as the model behind it, and the query vectors are generated separately
// against eval.S2Model, so two models in one cache means the vector arm — the
// baseline the whole graph delta is measured against — is nonsense.
//
// The empty model is its own case and not a mismatch. It is what a record written
// before eval.S2Record carried the field reads back as, so it says the provenance
// was never recorded, which is weaker than S2Model and different from wrong.
func reportModels(models map[string]int) {
	for _, m := range sortedKeys(models) {
		switch m {
		case eval.S2Model:
			log.Printf("  model      %-24s %d", m, models[m])
		case "":
			log.Printf("  model      %-24s %d  no provenance recorded (cached before the model "+
				"field existed); rerun prepare against a fresh %s to restore it",
				"(unrecorded)", models[m], s2File)
		default:
			log.Printf("  model      %-24s %d  WARNING: not %s, so these vectors are in a "+
				"different embedding space from the query vectors", m, models[m], eval.S2Model)
		}
	}
}

// s2Cache is everything one pass over the append-only cache tells prepare about
// what it already holds. It is a struct rather than four return values because
// each field answers a question that only this pass can answer, and they kept
// accumulating.
type s2Cache struct {
	// keys are the documents already asked about, whatever the answer was.
	keys map[string]bool

	// models tallies the embedding model behind every cached vector.
	models map[string]int

	// joined counts records carrying a CorpusId whose key belongs to the corpus the
	// scan was given. It is the only evidence in the file that the metadata join has
	// ever worked, which is what separates "every joinable document is already cached"
	// from "this metadata release does not match this corpus" — see the ErrEmptyDataset
	// guard in prepare.
	//
	// Scoped to the corpus rather than counted over the whole file, because the guard
	// asks about this corpus and an unscoped count answers about some other one. An
	// s2.jsonl kept from an earlier dataset satisfies it with records whose keys the
	// current corpus has never heard of: none of the current keys match the metadata,
	// the guard sees what looks like prior evidence of a join and stands down, and
	// every current document is written as a tombstone. build reads that as complete
	// coverage and publishes vector and graph arms over an index holding neither —
	// the docs/EVAL.md section 4.1 failure, rebuilt out of the check meant to stop it.
	joined int

	// good is the byte offset just past the last record that decoded cleanly.
	good int64
}

// scanS2Cache reads what is already in an append-only S2Record JSONL.
//
// A truncated final line is tolerated rather than fatal: it is the expected shape
// of an interrupted run, and the key it belongs to simply gets fetched again. The
// offset is what lets the caller drop that fragment before appending, which is the
// difference between a resumable file and one that quietly stops being readable.
//
// The tallies ride along on this pass because it is the only pass prepare makes
// over what it already holds, and a fact a resumed run cannot see is a fact nothing
// checks — see eval.S2Record.Model. Records with no vector are not counted in
// models: they belong to no embedding space.
//
// inCorpus is the key set of the corpus this cache is being scanned for, and only
// the joined tally consults it — keys, models and the offset describe the file
// whatever corpus it came from. A nil set is "no corpus to speak of", which leaves
// joined at zero rather than counting evidence about a corpus nobody named.
func scanS2Cache(path string, inCorpus map[string]bool) (s2Cache, error) {
	c := s2Cache{keys: map[string]bool{}, models: map[string]int{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return s2Cache{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	var damaged error
	for {
		var rec eval.S2Record
		if err := dec.Decode(&rec); err != nil {
			// EOF, or a record that did not decode. Both mean "stop reading", and
			// neither invalidates what came before: every earlier record was
			// followed by a newline before the next one began. Whether the second
			// case is a half-written tail or damage in the middle of the file is
			// not decidable here — the decoder reports both as the same error — so
			// it is settled after the loop, before the caller acts on c.good.
			if !errors.Is(err, io.EOF) {
				damaged = err
			}
			break
		}
		c.good = dec.InputOffset()
		if rec.Key != "" {
			c.keys[rec.Key] = true
		}
		if rec.CorpusID != "" && inCorpus[rec.Key] {
			c.joined++
		}
		if len(rec.Vector) > 0 {
			c.models[rec.Model]++
		}
	}

	// InputOffset stops at the closing brace of the last value it decoded, one byte
	// short of the newline the encoder wrote after it. Left there, a healthy complete
	// file looks one byte longer than its last good record and the caller truncates
	// that newline away as a fragment; the next appended record then lands on the
	// same line, producing `}{`. Go's stream decoder reads that back happily, so the
	// damage stays invisible here and surfaces in the line-oriented readers —
	// testdata/gen_query_vectors.py --verify calls json.loads per line and fails with
	// "extra data" on a file this command reports as fine.
	//
	// Advancing over the whitespace that follows fixes both cases at once, because
	// the separator and the fragment are distinguishable: whitespace after the last
	// value is the separator and belongs to what has been written, anything else is
	// the start of a record that never finished.
	if c.good > 0 {
		if _, err := f.Seek(c.good, io.SeekStart); err != nil {
			return s2Cache{}, fmt.Errorf("seek %s: %w", path, err)
		}
		var buf [16]byte
		n, err := f.Read(buf[:])
		if err != nil && !errors.Is(err, io.EOF) {
			return s2Cache{}, fmt.Errorf("read %s: %w", path, err)
		}
		for _, b := range buf[:n] {
			if b != '\n' && b != '\r' && b != ' ' && b != '\t' {
				break
			}
			c.good++
		}
	}

	// What kind of undecodable thing stopped the loop, decided before the caller acts
	// on c.good. prepare's response to a short offset is to truncate the file to it,
	// which is right for the half-written tail of an interrupted run and catastrophic
	// for anything else: damage in the middle deletes every valid record after it, and
	// those are the hours of rate-limited fetching this cache exists to avoid repeating.
	// It would go unremarked, too — the log line calls it a trailing record either way,
	// and the next run simply refetches what it no longer has.
	//
	// The encoder writes one record per line and escapes newlines inside strings, so a
	// record that never finished writing is the last line of the file and holds no
	// newline of its own. A newline anywhere past c.good therefore means a line that
	// did finish, and that is not something to throw away without being asked.
	if damaged != nil {
		if _, err := f.Seek(c.good, io.SeekStart); err != nil {
			return s2Cache{}, fmt.Errorf("seek %s: %w", path, err)
		}
		// Scanned in a fixed buffer rather than read whole: the remainder is one short
		// record when the answer is "tail", and there is no bound on it when the answer
		// is "damage" — which is the case that must not also exhaust memory.
		scan := make([]byte, 32<<10)
		for {
			n, err := f.Read(scan)
			if bytes.IndexByte(scan[:n], '\n') >= 0 {
				return s2Cache{}, fmt.Errorf("%s: the record at byte %d did not decode and complete "+
					"lines follow it, so this is damage in the middle of the cache rather than the "+
					"half-written tail of an interrupted run (%d records read before it). Resuming "+
					"truncates to byte %d, which here would delete every valid record after the "+
					"damage, so it is refused: repair or remove that one line, or delete %s and "+
					"refetch: %w", path, c.good, len(c.keys), c.good, path, damaged)
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return s2Cache{}, fmt.Errorf("read %s: %w", path, err)
			}
		}
		log.Printf("%s: ignoring incomplete trailing record (%v)", path, damaged)
	}
	return c, nil
}

func estimate(docs int) time.Duration {
	batches := (docs + eval.S2BatchLimit - 1) / eval.S2BatchLimit
	// 18s observed per healthy 500-id request carrying SPECTER vectors.
	return (time.Duration(batches) * 18 * time.Second).Round(time.Minute)
}

// ---------------------------------------------------------------- build

func build(ctx context.Context, args []string) error {
	var partial, anySnapshot bool
	data, flagErr := dataFlags(cmdBuild, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&partial, "partial", false,
			"build even though the Semantic Scholar cache does not cover every document "+
				"(the vector and graph arms then measure a corpus that is partly text-only)")
		snapshotFlag(fs, &anySnapshot)
	})
	if flagErr != nil {
		return flagErr
	}

	corpusPath := filepath.Join(*data, corpusFile)
	s2Path := filepath.Join(*data, s2File)
	dir := filepath.Join(*data, indexDir)

	// Checked here as well as in prepare, because the index is what the arms are
	// measured over and a corpus file that changed between the two would produce one
	// silently built from different documents than the cache was fetched for.
	if !anySnapshot {
		if err := verifySnapshot(*data, corpusFile); err != nil {
			return err
		}
	}

	// The other half of the corpus check, and the half this command cannot verify
	// for itself: the cache's own inputs. build never reads metadata.csv — the join
	// already happened — so the only evidence about the release behind these edges
	// and vectors is what prepare left. Read before the build rather than after, so
	// an operator who is about to spend minutes indexing sees it now, and carried
	// into the provenance record below so a later `run` acts on it rather than
	// trusting the log was read. See s2UnpinnedFile.
	preparedUnpinned := false
	if _, err := os.Stat(filepath.Join(*data, s2UnpinnedFile)); err == nil {
		preparedUnpinned = true
		log.Printf("WARNING: %s was joined with `prepare -any-snapshot`, so its edges and vectors",
			s2Path)
		log.Printf("WARNING: come from inputs docs/EVAL.md does not pin. The index is recorded as")
		log.Printf("WARNING: unpublishable and `run` will refuse it without -any-snapshot.")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", filepath.Join(*data, s2UnpinnedFile), err)
	}

	// The Semantic Scholar side is loaded first so every document can be added
	// complete. Add takes a whole Document, and attaching links in a second pass
	// would mean either a mutable index or two copies of the corpus.
	recs, err := readS2Records(s2Path)
	if err != nil {
		return err
	}
	log.Printf("s2: %d records", len(recs))

	// The corpus keys, in a pass of their own before anything is indexed. Two things
	// below need to know what "in this corpus" means and cannot wait for the streaming
	// pass to finish: the coverage gate, so a cache that does not cover the corpus
	// costs a message rather than a full build, and the reverse index, which must not
	// let a record from outside the corpus claim a CorpusId. A second read of
	// corpus.jsonl is a few seconds against a build measured in minutes.
	corpusKeys := make(map[string]bool)
	if err := eval.ReadCorpus(corpusPath, func(d eval.CorpusDoc) error {
		corpusKeys[d.ID] = true
		return nil
	}); err != nil {
		return err
	}

	var uncached int
	for key := range corpusKeys {
		if _, ok := recs[key]; !ok {
			uncached++
		}
	}

	// A document the cache says nothing about is not the same as a document the cache
	// says has nothing. prepare writes a record for every key it asks about, including
	// a bare tombstone for the ones with no Semantic Scholar side at all, so a finished
	// prepare covers the corpus exactly. A key missing from the cache therefore means
	// prepare did not finish — `-limit`, an interrupt, or a cache from an older corpus
	// — and the document silently enters the index with no vector and no links.
	//
	// Nothing downstream can tell that apart from a document the graph genuinely cannot
	// reach, and the run still labels its arms text+vector and text+vector+graph. That
	// is precisely how docs/EVAL.md section 4.1 produced a confident measurement of a
	// corpus that was 40% absent, so it fails here, before a document is tokenised
	// rather than after the commit. -partial is for deliberately smoke-testing the
	// pipeline on a slice.
	if uncached > 0 {
		pct := 100 * float64(uncached) / float64(len(corpusKeys))
		if !partial {
			return fmt.Errorf("%d of %d corpus documents (%.1f%%) have no record in %s, so they would "+
				"be indexed with no vector and no links while the run still reports vector and graph "+
				"arms; finish `weft-eval prepare` (it resumes), or pass -partial to build the slice "+
				"deliberately", uncached, len(corpusKeys), pct, s2Path)
		}
		log.Printf("WARNING: -partial: %d of %d documents (%.1f%%) are text-only because %s has no",
			uncached, len(corpusKeys), pct, s2Path)
		log.Printf("WARNING: record for them. The vector and graph arms measured on this index are")
		log.Printf("WARNING: NOT comparable to a full build and must not be published.")
	}

	byCorpusID, duplicated, foreign := corpusIDIndex(recs, corpusKeys)
	log.Printf("s2: %d resolvable corpus ids (%d further records share one of them; "+
		"the lowest cord_uid holds the mapping)", len(byCorpusID), duplicated)
	if foreign > 0 {
		log.Printf("s2: %d records are for documents outside this corpus and hold no mapping "+
			"(cache from a larger or older corpus)", foreign)
	}

	ix := engine.New()
	// Commit adopts the segment it publishes, so from that point this index holds
	// mappings — and a mapping is not memory the collector owns. Deferred here
	// rather than after the Commit so every return path below releases it, and
	// harmless before then: an index that never mapped anything closes free.
	defer ix.Close() //nolint:errcheck // nothing left to do about it on the way out
	var added, withVec, withLinks, links, dangling, repeated, dimSkipped int

	// The width every vector is checked against, decided over the whole cache before
	// a document is indexed rather than by whichever vector the corpus order happens
	// to present first. Taking the first one makes a single malformed response
	// authoritative and skips every correct vector behind it — and the build still
	// commits, so the run goes on to publish a vector arm backed by the one outlier.
	// That is the same shape as docs/EVAL.md section 4.1: a confident number over a
	// corpus that is not there.
	dim, dimTally := dominantDim(recs, corpusKeys)

	// And the embedding space those widths belong to, which width cannot tell apart.
	// See checkVectorModels: two SPECTER releases share 768 dimensions and mean
	// different things by them.
	if err := checkVectorModels(recs, corpusKeys, partial); err != nil {
		return err
	}

	err = eval.ReadCorpus(corpusPath, func(d eval.CorpusDoc) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		doc := engine.Document{
			Key: d.ID,
			// Title and abstract concatenated. BEIR scores trec-covid over both, and
			// a title-only index would understate the text baseline the graph delta
			// is measured against.
			Text: d.Title + " " + d.Text,
		}
		// Absent only under -partial: the gate above already refused an incomplete
		// cache, so on a publishable build every document has a record.
		if r, cached := recs[d.ID]; cached {
			if n := len(r.Vector); n > 0 {
				if n == dim {
					doc.Vector = r.Vector
					withVec++
				} else {
					// engine.Add would reject this with ErrDimMismatch and abort the
					// whole build. One inconsistent vector is not worth losing a
					// 171K-document index over; it is worth counting.
					dimSkipped++
				}
			}
			for _, ref := range r.Refs {
				key, ok := byCorpusID[ref]
				if !ok {
					dangling++
					continue
				}
				// An edge appears once. A references list can name the same CorpusId
				// twice, and duplicate-paper merging on the Semantic Scholar side can
				// resolve a reference back to this very document — either would be
				// counted below as an edge the graph does not have, because the
				// traversal dedupes on visit and a self-edge leads nowhere. The count
				// is what makes it matter: docs/EVAL.md publishes an in-corpus edge
				// total as a description of the graph the arms traverse, so an inflated
				// one is a claim about density that no ranking can corroborate or
				// contradict. Linear scan because a references list is short and this
				// runs once per document, not once per query.
				if key == d.ID || slices.Contains(doc.Links, key) {
					repeated++
					continue
				}
				doc.Links = append(doc.Links, key)
				links++
			}
			if len(doc.Links) > 0 {
				withLinks++
			}
			// Released as it is consumed. Nothing reads recs after this pass —
			// corpusIDIndex and dominantDim both ran above — and Add clones the vector
			// into the index, so holding the cache entry as well keeps two copies of
			// every SPECTER vector alive: about 525 MB of the 171K-document build,
			// doubled, for a map nobody will look at again.
			delete(recs, d.ID)
		}
		if _, err := ix.Add(doc); err != nil {
			return err
		}
		added++
		return nil
	})
	if err != nil {
		return err
	}

	docs, avgdl := ix.Stats()
	log.Printf("index: %d documents, avgdl %.1f", docs, avgdl)
	log.Printf("  vectors    %d (dim %d, %d skipped for width)", withVec, dim, dimSkipped)
	if len(dimTally) > 1 {
		// Named rather than merely counted. More than one width in one cache means
		// more than one embedding space, which is the same measurement failure
		// reportModels warns about arriving by a different route — and the minority
		// is skipped, so the vector arm covers fewer documents than the count above
		// suggests it might.
		log.Printf("WARNING: %s holds %d vector widths %v; only the %d-dim ones are indexed,",
			s2Path, len(dimTally), dimTally, dim)
		log.Printf("WARNING: so %d documents are text-only in a corpus reported as covered.", dimSkipped)
	}
	log.Printf("  linked     %d documents, %d in-corpus edges", withLinks, links)
	log.Printf("  dangling   %d references outside the corpus (skipped at traversal by design)", dangling)
	if repeated > 0 {
		// Reported rather than silently dropped, because the number says something
		// about the source data: it is how many references resolved to an edge this
		// document already had, or to the document itself. Counted separately from
		// dangling — those point outside the corpus, these point at a place the
		// traversal reaches anyway.
		log.Printf("  repeated   %d references collapsed into an edge that already existed (or into a self-edge)", repeated)
	}

	// No MkdirAll here: the next two steps clear this directory and Commit
	// creates it, owner-only, on its way in.
	//
	// The old account of this index goes before the new index does.
	//
	// A rebuild replaces the segments and then records what they came from, and the
	// gap between those two is a window in which a crash leaves the previous
	// provenance.json standing beside a manifest it does not describe. A later run
	// verifies the stale record and accepts a foreign or partial index as the pinned
	// one — the exact substitution provenance exists to refuse, reached through
	// provenance itself. Removed first, so a crash anywhere in the window leaves an
	// index that cannot say what it holds, and verifyProvenance refuses that.
	if err := os.Remove(filepath.Join(dir, provenanceFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale %s: %w", provenanceFile, err)
	}
	// Clear any previous index before committing over it. A commit is
	// incremental now — it appends a segment to whatever the directory already
	// holds — so committing a freshly built index into a populated directory is
	// refused rather than silently overlapping the DocIDs. This command builds
	// an index from the sources named in provenance.json, which is a
	// replacement and not an addition, so it says so.
	//
	// Ordered after the provenance removal on purpose: the window a crash can
	// land in already leaves an index that cannot say what it holds, and
	// widening it to also leave no index is no worse.
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear %s before rebuilding: %w", dir, err)
	}
	start := time.Now()
	if err := ix.Commit(dir); err != nil {
		return err
	}
	log.Printf("committed to %s in %s", dir, time.Since(start).Round(time.Millisecond))

	// Reopening here rather than trusting the write is the first real-corpus
	// exercise of milestone 2's restore equivalence — until now it had only been
	// tested on fixtures.
	//
	// The built index is released first. Its Commit adopted the segment it just
	// wrote, so holding it across the reopen means two mappings of the same
	// corpus — and on the platforms mapFile reads instead of maps, two complete
	// copies on the heap. Nothing below reads it.
	if err := ix.Close(); err != nil {
		return fmt.Errorf("close before reopening: %w", err)
	}
	start = time.Now()
	reopened, err := engine.Open(dir)
	if err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	defer reopened.Close() //nolint:errcheck // nothing left to do about it on the way out
	rdocs, ravg := reopened.Stats()
	if rdocs != docs || ravg != avgdl {
		return fmt.Errorf("reopened index has %d documents at avgdl %v, committed %d at %v",
			rdocs, ravg, docs, avgdl)
	}
	log.Printf("reopened in %s: %d documents, avgdl %.1f — matches",
		time.Since(start).Round(time.Millisecond), rdocs, ravg)

	// What this index is, written into it. Hashed here rather than taken from the
	// snapshot table even when the check above passed, so the record describes the
	// file that was read instead of the one that was expected — and so an
	// -any-snapshot build says which corpus it used rather than saying nothing. The
	// read costs about a second against a build measured in minutes.
	corpusSHA, err := sha256File(corpusPath)
	if err != nil {
		return err
	}
	if err := writeProvenance(dir, provenance{
		Corpus:          corpusSHA,
		Partial:         partial,
		PrepareUnpinned: preparedUnpinned,
	}); err != nil {
		return err
	}
	return nil
}

// corpusIDIndex inverts the records into CorpusId -> cord_uid, so a reference can be
// expressed as a Document.Link, and reports how many records lost that position to an
// earlier one. Links are keyed by document key by design (docs/DATASETS.md section
// 4), and that is exactly what makes this join possible: a DocID adjacency list could
// not have been filled in from an external graph.
//
// Built in sorted cord_uid order, not map order, and first writer wins. The mapping
// is not injective: CORD-19 ships the same paper under several cord_uids — 20,556 of
// 162,837 resolvable ids on the release measured in docs/EVAL.md — so most of those
// CorpusIds have more than one candidate. Ranging over the map directly let Go's
// randomised iteration pick between them, which meant every citation to a duplicated
// paper landed on a different copy from one build to the next: a different graph,
// different rankings and different published numbers out of the same cache. Which
// copy wins matters far less than that the same one always does; duplicates carry
// near-identical text, and reproducibility is what this harness is for.
//
// Only records whose key is in corpus are eligible, and the count of the rest is
// returned. A cache from a larger or older corpus can cover every current key and
// still hold records for documents that are not here any more, and those keys take
// part in the same sorted first-writer-wins race: one of them sorting below the
// present copy is enough to hand the mapping to a key the index does not hold. Every
// citation to that paper then resolves to nothing, and real in-corpus edges are
// counted as dangling and dropped — a quietly sparser graph, from a cache the
// coverage gate above accepts as complete because it does cover the corpus.
func corpusIDIndex(recs map[string]eval.S2Record, corpus map[string]bool) (byCorpusID map[string]string, duplicated, foreign int) {
	byCorpusID = make(map[string]string, len(recs))
	for _, key := range sortedKeys(recs) {
		if !corpus[key] {
			foreign++
			continue
		}
		id := recs[key].CorpusID
		if id == "" {
			continue
		}
		if _, seen := byCorpusID[id]; seen {
			duplicated++
			continue
		}
		byCorpusID[id] = key
	}
	return byCorpusID, duplicated, foreign
}

// dominantDim is the vector width held by the most in-corpus records, and the tally
// behind it.
//
// Only records whose key is in the corpus vote. A cache built for a larger or older
// corpus can carry vectors for documents that are not here any more, and those are not
// evidence about the width this build should index at.
//
// A tie goes to the narrower width, which is arbitrary and only has to be stable: two
// builds over one cache have to agree, and a tie means the cache holds two embedding
// spaces in equal measure, which is a thing for the caller to report rather than a
// thing to pick well. The zero return for a cache with no vectors at all is the width
// no record matches, so nothing is indexed with a vector — which is correct, and which
// the coverage line then shows as 0.
// checkVectorModels refuses to index document vectors that a foreign embedding model
// produced, and reports the provenance of the ones it does index.
//
// Width is not provenance. SPECTER v1 and v2 both emit 768 dimensions, so dominantDim
// accepts either and engine.Add stores either; the query vectors, meanwhile, come from
// testdata/gen_query_vectors.py under eval.S2Model. Cosine similarity between two
// embedding spaces is a number with no meaning, and it arrives as a plausible-looking
// vector baseline and a graph delta measured against it. Nothing downstream could
// notice: the model label is not part of a committed index, and prepare's warning is
// printed by the invocation that fetched the batch, which on a resumed job is not the
// one that finishes.
//
// Refused rather than skipped. A skipped vector lowers coverage, and coverage is
// already reported, so a run could publish a vector arm over whatever remained without
// the reason ever being stated — docs/EVAL.md section 4.1 is what that looks like.
// -partial is the existing way to say "these arms are not publishable" and is the
// override here too; run, sweep and weights refuse a partial index by its provenance.
//
// An unrecorded model is warned about and indexed. It means a cache written before the
// field existed — which is what the committed measurement was fetched into, all 148,232
// of its vectors — so refusing it would refuse the published index rather than a
// mistake. The warning is the honest statement of what is and is not known.
func checkVectorModels(recs map[string]eval.S2Record, corpus map[string]bool, partial bool) error {
	tally := map[string]int{}
	for key, r := range recs {
		if corpus[key] && len(r.Vector) > 0 {
			tally[r.Model]++
		}
	}
	reportModels(tally)

	foreign, worst := 0, ""
	for m, n := range tally {
		if m == eval.S2Model || m == "" {
			continue
		}
		foreign += n
		// Named deterministically: map order is randomised, and an error that changes
		// between identical runs is an error nobody can act on.
		if worst == "" || m < worst {
			worst = m
		}
	}
	if foreign == 0 {
		return nil
	}
	if partial {
		log.Printf("WARNING: indexing %d vectors from %s under -partial; they are in a different "+
			"embedding space from the query vectors, so the vector and graph arms measure nothing",
			foreign, worst)
		return nil
	}
	return fmt.Errorf("%d cached vectors were produced by %s rather than %s, and the query vectors "+
		"are %s: cosine similarity across two embedding spaces is not a similarity, and an index "+
		"carries no model label for a later command to check. Refetch those documents against a "+
		"fresh %s, or pass -partial to build an index whose arms must not be published",
		foreign, worst, eval.S2Model, eval.S2Model, s2File)
}

func dominantDim(recs map[string]eval.S2Record, corpus map[string]bool) (dim int, tally map[int]int) {
	tally = make(map[int]int)
	for key, r := range recs {
		if corpus[key] && len(r.Vector) > 0 {
			tally[len(r.Vector)]++
		}
	}
	best := 0
	for n, count := range tally {
		// tally[best] is 0 on the first iteration, so the first width always wins.
		if count > tally[best] || (count == tally[best] && n < best) {
			best = n
		}
	}
	return best, tally
}

func readS2Records(path string) (map[string]eval.S2Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w (run `weft-eval prepare` first)", path, err)
	}
	defer f.Close()

	out := make(map[string]eval.S2Record)
	dec := json.NewDecoder(f)
	for {
		var rec eval.S2Record
		err := dec.Decode(&rec)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// A value that runs off the end of the file: the half-written trailing
			// record an interrupted run leaves behind. Nothing follows it, so nothing
			// is lost but the one key, which prepare refetches.
			log.Printf("%s: ignoring incomplete trailing record (%v)", path, err)
			break
		}
		if err != nil {
			// Anything else is malformed mid-file, and is not swallowed. The decoder
			// stops where it stands, so every record after it would be invisible — and
			// the failure mode this harness is built against is exactly a run that
			// completes over a quietly smaller corpus and reports a lower nDCG that
			// reads as a worse ranking.
			return nil, fmt.Errorf("%s: record %d ends at byte %d: %w: %v (rerun `weft-eval prepare` to repair)",
				path, len(out)+1, dec.InputOffset(), eval.ErrBadRecord, err)
		}
		if rec.Key != "" {
			// A repeated key is refused rather than overwritten. prepare emits exactly one
			// record per document — it skips what the cache already holds — so a second
			// record for one key means the file was assembled from more than one cache, and
			// the later one wins by nothing but file position. That is not a harmless
			// duplicate: a tombstone written after a full record discards a vector and a
			// reference list while the coverage gate still counts the document as cached, so
			// the graph and the vector arm both shrink with nothing in the log to show it.
			if _, dup := out[rec.Key]; dup {
				return nil, fmt.Errorf("%s: %q appears twice: %w (two caches concatenated? "+
					"rerun `weft-eval prepare` against one of them)", path, rec.Key, eval.ErrBadRecord)
			}
			out[rec.Key] = rec
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: %w", path, eval.ErrEmptyDataset)
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
