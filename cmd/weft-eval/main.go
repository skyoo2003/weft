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
	done, good, err := doneKeys(outPath)
	if err != nil {
		return err
	}
	if len(done) > 0 {
		log.Printf("resume: %d documents already in %s", len(done), outPath)
	}

	// A hard kill can leave a half-written trailing record. Reading tolerates that,
	// but appending after it would splice the fragment into the next record and make
	// everything written from here on unreadable — silently, and only discovered by
	// the next resume, which would then refetch hours of work it already has. Drop
	// the fragment instead.
	if fi, err := os.Stat(outPath); err == nil && fi.Size() > good {
		log.Printf("%s: dropping %d trailing bytes of an incomplete record", outPath, fi.Size()-good)
		if err := os.Truncate(outPath, good); err != nil {
			return fmt.Errorf("truncate %s: %w", outPath, err)
		}
	}

	want := make(map[string]bool, len(uids))
	for _, uid := range uids {
		if !done[uid] {
			want[uid] = true
		}
	}
	if len(want) == 0 {
		log.Printf("nothing to fetch; %s is complete", outPath)
		return nil
	}

	// 3. The join. ReadCORD19IDs checks the header before reading a single row, so
	// a release that cannot be joined at all fails immediately instead of after
	// scanning 1.6 GB.
	log.Printf("scanning %s for %d cord_uids...", metaPath, len(want))
	ids, tally, err := eval.ReadCORD19IDs(metaPath, want)
	if err != nil {
		return err
	}
	log.Printf("join: %d of %d resolved to an identifier", len(ids), len(want))
	for _, k := range sortedKeys(tally) {
		log.Printf("  %-16s %d", k, tally[k])
	}
	if unresolved := len(want) - len(ids); unresolved > 0 {
		log.Printf("  %-16s %d (no usable identifier; they stay in the corpus with no edges or vector)",
			"unresolved", unresolved)
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

	client := eval.NewS2Client(apiKey, log.Printf)
	batches := (len(targets) + eval.S2BatchLimit - 1) / eval.S2BatchLimit
	if apiKey == "" {
		log.Printf("no API key: pacing at %s per request, %d batches to go (~%s)",
			client.Pace, batches, estimate(len(targets)))
	}

	var fetched, missing, withVec, withRefs, rejected, edges int
	models := make(map[string]int)
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
					withVec++
					models[p.Model]++
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
	// Provenance, printed rather than assumed. Every published vector number is only
	// as comparable as the model behind it, and the query vectors are generated
	// separately against eval.S2Model.
	for _, m := range sortedKeys(models) {
		if m == eval.S2Model {
			log.Printf("  model      %-24s %d", m, models[m])
			continue
		}
		log.Printf("  model      %-24s %d  WARNING: not %s, so these vectors are in a "+
			"different embedding space from the query vectors", m, models[m], eval.S2Model)
	}
	log.Printf("  citing     %d documents, %d edges", withRefs, edges)
	return nil
}

// doneKeys reads the keys already present in an append-only S2Record JSONL, and
// returns the byte offset just past the last record that decoded cleanly.
//
// A truncated final line is tolerated rather than fatal: it is the expected shape
// of an interrupted run, and the key it belongs to simply gets fetched again. The
// offset is what lets the caller drop that fragment before appending, which is the
// difference between a resumable file and one that quietly stops being readable.
func doneKeys(path string) (map[string]bool, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	done := make(map[string]bool)
	var good int64
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
		good = dec.InputOffset()
		if rec.Key != "" {
			done[rec.Key] = true
		}
	}
	return done, good, nil
}

func estimate(docs int) time.Duration {
	batches := (docs + eval.S2BatchLimit - 1) / eval.S2BatchLimit
	// 18s observed per healthy 500-id request carrying SPECTER vectors.
	return (time.Duration(batches) * 18 * time.Second).Round(time.Minute)
}

// ---------------------------------------------------------------- build

func build(ctx context.Context, args []string) error {
	data := dataFlags("build", args, nil)

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

	// CorpusId -> cord_uid, so a reference can be expressed as a Document.Link.
	// Links are keyed by document key by design (docs/DATASETS.md section 4), and
	// that is exactly what makes this join possible: a DocID adjacency list could
	// not have been filled in from an external graph.
	byCorpusID := make(map[string]string, len(recs))
	for _, r := range recs {
		if r.CorpusID != "" {
			byCorpusID[r.CorpusID] = r.Key
		}
	}
	log.Printf("s2: %d resolvable corpus ids", len(byCorpusID))

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
		if r, ok := recs[d.ID]; ok {
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
