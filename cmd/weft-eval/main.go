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

	// queryVecFile is written by internal/eval/testdata/gen_query_vectors.py. It is
	// optional; run reports its absence rather than quietly measuring a text-only
	// baseline.
	queryVecFile = "query-vectors.jsonl"
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
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "prepare":
		err = prepare(ctx, args)
	case "build":
		err = build(ctx, args)
	case "diagnose":
		err = diagnose(ctx, args)
	case "run":
		err = run(ctx, args)
	case "sweep":
		err = sweep(ctx, args)
	case "weights":
		err = weights(ctx, args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "weft-eval: unknown subcommand %q\n", cmd)
		usage()
	}
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

// dataFlags are shared by every subcommand.
func dataFlags(name string, args []string, extra func(*flag.FlagSet)) *string {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	dir := fs.String("data", ".eval-data", "directory holding the downloaded corpus and generated artifacts")
	if extra != nil {
		extra(fs)
	}
	_ = fs.Parse(args) // ExitOnError already handled a bad flag.
	return dir
}

// ---------------------------------------------------------------- prepare

func prepare(ctx context.Context, args []string) error {
	var apiKey string
	var limit int
	data := dataFlags("prepare", args, func(fs *flag.FlagSet) {
		fs.StringVar(&apiKey, "api-key", os.Getenv("S2_API_KEY"),
			"Semantic Scholar API key (optional; raises the rate limit)")
		fs.IntVar(&limit, "limit", 0,
			"stop after this many documents (0 = all); for smoke-testing the pipeline")
	})

	corpusPath := filepath.Join(*data, corpusFile)
	metaPath := filepath.Join(*data, metadataFile)
	outPath := filepath.Join(*data, s2File)

	// 1. Corpus ids. Reading these first means a missing or truncated corpus fails
	// before an hours-long fetch rather than during it.
	var uids []string
	if err := eval.ReadCorpus(corpusPath, func(d eval.CorpusDoc) error {
		uids = append(uids, d.ID)
		return nil
	}); err != nil {
		return err
	}
	log.Printf("corpus: %d documents", len(uids))

	// 2. Resume. The output is append-only JSONL, so what is already in it is
	// exactly what does not need refetching.
	cache, err := scanS2Cache(outPath)
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

	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
	log.Printf("  vectors    %d (%d rejected as non-finite)", withVec, rejected)
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

	// joined counts records carrying a CorpusId. It is the only evidence in the file
	// that the metadata join has ever worked, which is what separates "every joinable
	// document is already cached" from "this metadata release does not match this
	// corpus" — see the ErrEmptyDataset guard in prepare.
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
func scanS2Cache(path string) (s2Cache, error) {
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
	for {
		var rec eval.S2Record
		if err := dec.Decode(&rec); err != nil {
			// EOF, or a half-written trailing record. Both mean "stop reading", and
			// neither invalidates what came before: every earlier record was
			// followed by a newline before the next one began.
			if !errors.Is(err, io.EOF) {
				log.Printf("%s: ignoring incomplete trailing record (%v)", path, err)
			}
			break
		}
		c.good = dec.InputOffset()
		if rec.Key != "" {
			c.keys[rec.Key] = true
		}
		if rec.CorpusID != "" {
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
	return c, nil
}

func estimate(docs int) time.Duration {
	batches := (docs + eval.S2BatchLimit - 1) / eval.S2BatchLimit
	// 18s observed per healthy 500-id request carrying SPECTER vectors.
	return (time.Duration(batches) * 18 * time.Second).Round(time.Minute)
}

// ---------------------------------------------------------------- build

func build(ctx context.Context, args []string) error {
	var partial bool
	data := dataFlags("build", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&partial, "partial", false,
			"build even though the Semantic Scholar cache does not cover every document "+
				"(the vector and graph arms then measure a corpus that is partly text-only)")
	})

	corpusPath := filepath.Join(*data, corpusFile)
	s2Path := filepath.Join(*data, s2File)
	dir := filepath.Join(*data, indexDir)

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
	var added, withVec, withLinks, links, dangling, dimSkipped int
	dim := 0

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
				if dim == 0 {
					dim = n
				}
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
				if key, ok := byCorpusID[ref]; ok {
					doc.Links = append(doc.Links, key)
					links++
				} else {
					dangling++
				}
			}
			if len(doc.Links) > 0 {
				withLinks++
			}
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
	log.Printf("  linked     %d documents, %d in-corpus edges", withLinks, links)
	log.Printf("  dangling   %d references outside the corpus (skipped at traversal by design)", dangling)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	start := time.Now()
	if err := ix.Commit(dir); err != nil {
		return err
	}
	log.Printf("committed to %s in %s", dir, time.Since(start).Round(time.Millisecond))

	// Reopening here rather than trusting the write is the first real-corpus
	// exercise of milestone 2's restore equivalence — until now it had only been
	// tested on fixtures.
	start = time.Now()
	reopened, err := engine.Open(dir)
	if err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	rdocs, ravg := reopened.Stats()
	if rdocs != docs || ravg != avgdl {
		return fmt.Errorf("reopened index has %d documents at avgdl %v, committed %d at %v",
			rdocs, ravg, docs, avgdl)
	}
	log.Printf("reopened in %s: %d documents, avgdl %.1f — matches",
		time.Since(start).Round(time.Millisecond), rdocs, ravg)
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
func corpusIDIndex(recs map[string]eval.S2Record, corpus map[string]bool) (map[string]string, int, int) {
	byCorpusID := make(map[string]string, len(recs))
	var duplicated, foreign int
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
