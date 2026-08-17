// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/skyoo2003/weft/internal/eval"
	"github.com/skyoo2003/weft/pkg/engine"
)

// recall is milestone 3b's second and third acceptance assertions, in one
// command: what the approximate vector index gives up, and what it buys.
//
// It exists because the numbers the milestone publishes are not derivable from
// `run`. nDCG says whether the answer got worse and says nothing about why; a
// partition can lose half its neighbours and still hold nDCG steady if the ones
// it lost were unjudged. Recall against a brute-force scan is the question that
// separates "the approximation is fine" from "the approximation is bad and the
// benchmark cannot see it", and only the first of those is a result.
//
// The three things it prints, and why each is here rather than assumed:
//
//	recall@k        overlap with an exact scan of the same corpus, per query
//	candidates      how many documents a query actually scores
//	working set     bytes and distinct pages of `docs` a query touches
//
// The last is the one the milestone is named for. Milestone 3a moved 434 MiB of
// vectors off the Go heap and into the page cache without reducing them, and
// FINDINGS said so; section 1 of the 3b plan predicted the partition would take
// a query's working set to about 12 MiB. This measures it. A prediction that
// turns out wrong is a finding, and the plan registered the consequence in
// advance: a measured working set more than twice the prediction makes
// separating a `vectors` section from `docs` a debt with a trigger rather than
// an idea.

// recallPageSize is the granularity the working set is counted in. Not
// os.Getpagesize(): the number is published, and a figure that changes with the
// machine that produced it is not one docs/EVAL.md can print. 4 KiB is what
// x86-64 Linux and the CI runners use, and the arithmetic is stated so a reader
// on a 16 KiB machine can rescale it.
const recallPageSize = 4096

// segDirPrefix is what a segment directory is named with, from docs/FORMAT.md
// section 2. Counting them is the whole of this tool's interest in the layout —
// engine will not say how many segments an open index has, and the extent
// derivation below is only exact for one.
const segDirPrefix = "seg-"

func recall(ctx context.Context, args []string) error {
	var k, queries int
	var anySnapshot bool
	data, flagErr := dataFlags(cmdRecall, args, func(fs *flag.FlagSet) {
		fs.IntVar(&k, "k", frozenK, "rank cut the overlap is measured at")
		fs.IntVar(&queries, "queries", 0, "measure only the first N queries (0 = all); the brute-force scan is the slow half")
		snapshotFlag(fs, &anySnapshot)
	})
	if flagErr != nil {
		return flagErr
	}
	if k <= 0 {
		return fmt.Errorf("-k=%d: the rank cut must be positive", k)
	}
	if !anySnapshot {
		if err := verifySnapshot(*data, queriesFile, qrelsFile); err != nil {
			return err
		}
	}

	ix, err := openIndex(*data, anySnapshot)
	if err != nil {
		return err
	}
	defer ix.Close() //nolint:errcheck // nothing left to do about it on the way out
	qs, err := loadQueries(*data)
	if err != nil {
		return err
	}

	withVec, err := vectorQueries(qs, queries)
	if err != nil {
		return err
	}

	ext, err := recordExtents(ix, filepath.Join(*data, indexDir))
	if err != nil {
		return err
	}

	st, err := measureRecall(ctx, ix, ext, withVec, k)
	if err != nil {
		return err
	}
	docs, _ := ix.Stats()
	// Nothing to divide by. Every exact scan came back empty, which on a corpus
	// that has documents means their vectors are not the width the queries carry —
	// the mixed-embedding-model case. Printing NaN for the recall and +Inf for the
	// worst query would publish that as a measurement.
	if st.want == 0 {
		return fmt.Errorf("no query produced an exact top-%d over %d documents: the corpus carries no vector "+
			"of the queries' width, so there is no overlap to measure (see docs/EVAL.md section 7)", k, docs)
	}
	st.print(k, docs, ext.total)
	return nil
}

// recallStats is one run's measurements. Sums and extremes only: every average
// and every ratio is computed where it is printed, beside the label that says
// what it means.
type recallStats struct {
	n            int
	hits, want   int
	cands        int
	ivfTime      time.Duration
	bruteTime    time.Duration
	bytesTouched int
	pagesTouched int
	worstRecall  float64
	worstQuery   string
	rssBefore    int64
	rssAfter     int64
}

// measureRecall runs both passes over every query: what the partition answers,
// and the exact scan it is graded against.
func measureRecall(ctx context.Context, ix *engine.Index, ext extents, qs []queryVec, k int) (recallStats, error) {
	st := recallStats{n: len(qs), worstRecall: math.Inf(1)}
	// The exact scan's id list, built once. It is every id in the index and the
	// index does not change under this loop, so rebuilding it per query would put
	// a corpus-sized allocation inside the very timing it is being compared with.
	all := make([]engine.DocID, ix.Len())
	for i := range all {
		all[i] = engine.DocID(i)
	}

	st.rssBefore = maxRSS()
	for _, q := range qs {
		if err := ctx.Err(); err != nil {
			return recallStats{}, err
		}
		// The exact answer recall is measured against: cosine over every document,
		// no partition consulted. Scored here rather than through scorer/vector,
		// which now reads Index.Nearest — asking it for the exact answer would be
		// asking the approximation to grade itself.
		start := time.Now()
		exact := scoreTopK(ix, q.vec, all, k)
		st.bruteTime += time.Since(start)

		start = time.Now()
		ids := ix.Nearest(q.vec, k)
		approx := scoreTopK(ix, q.vec, ids, k)
		st.ivfTime += time.Since(start)

		overlap := 0
		for _, id := range exact {
			if slices.Contains(approx, id) {
				overlap++
			}
		}
		st.hits += overlap
		st.want += len(exact)
		st.cands += len(ids)
		b, p := workingSet(ext, ids)
		st.bytesTouched += b
		st.pagesTouched += p
		if len(exact) > 0 {
			if r := float64(overlap) / float64(len(exact)); r < st.worstRecall {
				st.worstRecall, st.worstQuery = r, q.id
			}
		}
	}
	st.rssAfter = maxRSS()
	return st, nil
}

// print is the report, and total is the whole docs section the working set is
// read against.
func (st recallStats) print(k, docs int, total int64) {
	n := float64(st.n)
	fmt.Printf("\nrecall@%d against a brute-force scan, %d queries with vectors, %d documents\n\n", k, st.n, docs)
	fmt.Printf("  %-34s %12.4f\n", "recall@"+fmt.Sprint(k), float64(st.hits)/float64(st.want))
	fmt.Printf("  %-34s %12.4f  (query %s)\n", "worst query", st.worstRecall, st.worstQuery)
	fmt.Printf("  %-34s %12.1f  of %d documents (%.2f%%)\n", "candidates per query",
		float64(st.cands)/n, docs, 100*float64(st.cands)/n/float64(docs))

	fmt.Printf("\nlatency per query\n\n")
	fmt.Printf("  %-34s %12s\n", "partitioned", (st.ivfTime / time.Duration(st.n)).Round(time.Microsecond))
	fmt.Printf("  %-34s %12s\n", "brute force", (st.bruteTime / time.Duration(st.n)).Round(time.Microsecond))
	if st.ivfTime > 0 {
		fmt.Printf("  %-34s %11.1fx\n", "speedup", float64(st.bruteTime)/float64(st.ivfTime))
	}

	fmt.Printf("\nworking set per query, docs section\n\n")
	fmt.Printf("  %-34s %12s\n", "record bytes reached", mib(float64(st.bytesTouched)/n))
	fmt.Printf("  %-34s %12s  (%.0f distinct %d B pages)\n", "pages reached",
		mib(float64(st.pagesTouched)/n*recallPageSize), float64(st.pagesTouched)/n, recallPageSize)
	fmt.Printf("  %-34s %12s\n", "whole docs section", mib(float64(total)))
	// Labelled for what it is. MaxRSS is a high-water mark and the loop above runs
	// a brute-force scan of the whole corpus beside every partitioned query, so
	// this is the bound the two passes reached together — not the partition's
	// working set. The two figures above it are the partition's, and that they are
	// derived rather than observed is exactly why they are the ones published.
	fmt.Printf("  %-34s %12s\n", "MaxRSS before", mib(float64(st.rssBefore)))
	fmt.Printf("  %-34s %12s\n", "MaxRSS after (both passes)", mib(float64(st.rssAfter)))
	fmt.Println()
}

func mib(b float64) string { return fmt.Sprintf("%.1f MiB", b/(1<<20)) }

// queryVec is a query that can be measured: an id to name the worst one with,
// and the vector the partition is asked about.
type queryVec struct {
	id  string
	vec []float32
}

// vectorQueries keeps the queries that carry a vector, at most limit of them.
//
// Without query-vectors.jsonl every query here would be a no-op and the command
// would print a perfect recall over zero measurements, which is the failure mode
// `run` already warns about for the vector arm — so an empty result is an error
// rather than a report.
func vectorQueries(qs []eval.Query, limit int) ([]queryVec, error) {
	var withVec []queryVec
	for _, q := range qs {
		if len(q.Query.Vector) > 0 {
			withVec = append(withVec, queryVec{q.ID, q.Query.Vector})
		}
	}
	if len(withVec) == 0 {
		return nil, fmt.Errorf("no query carries a vector: %s is missing or empty, so there is nothing to measure "+
			"the recall of (see docs/EVAL.md section 7)", queryVecFile)
	}
	if limit > 0 && limit < len(withVec) {
		withVec = withVec[:limit]
	}
	return withVec, nil
}

// scoreTopK ranks ids by cosine against q and returns the best k, with the same
// skips and the same DocID tiebreak scorer/vector applies.
func scoreTopK(ix *engine.Index, q []float32, ids []engine.DocID, k int) []engine.DocID {
	qn := vecNorm(q)
	if qn == 0 {
		return nil
	}
	cands := make([]engine.Candidate, 0, len(ids))
	for _, id := range ids {
		d, ok := ix.Doc(id)
		if !ok || len(d.Vector) != len(q) {
			continue
		}
		dn := vecNorm(d.Vector)
		if dn == 0 {
			continue
		}
		var sum float64
		for i, c := range q {
			sum += float64(c) * float64(d.Vector[i])
		}
		cands = append(cands, engine.Candidate{Doc: id, Score: sum / (qn * dn)})
	}
	top := engine.TopK(cands, k)
	out := make([]engine.DocID, len(top))
	for i, c := range top {
		out[i] = c.Doc
	}
	return out
}

func vecNorm(v []float32) float64 {
	var sum float64
	for _, c := range v {
		sum += float64(c) * float64(c)
	}
	return math.Sqrt(sum)
}

// extents is where each document's record sits inside the docs section, which is
// what turns a candidate list into a page count.
//
// The offsets are re-derived from the documents rather than read out of the
// docoff table, because docoff is engine's and this is a tool. Every field's
// encoded width is fixed by docs/FORMAT.md section 4, so the derivation is exact
// as long as that document is — and a drift would show up as a total that
// disagrees with the section's own size on disk, which is printed beside it.
//
// Exact for one segment, which is what the derivation assumes and what
// recordExtents therefore checks. DocIDs run on across a directory's segments
// but the docs files do not: each starts its own payload at offset zero, so a
// second segment's records would be laid out here on top of the first's, counted
// as sharing pages they cannot share, and the working set would print under the
// truth. `weft-eval build` publishes one commit and so one segment; an index
// holding more is refused rather than measured wrong.
type extents struct {
	off   []int64 // one per DocID, ascending
	size  []int64 // int64 rather than int32: a record is whatever a caller put in it
	total int64
}

func recordExtents(ix *engine.Index, path string) (extents, error) {
	ents, err := os.ReadDir(path)
	if err != nil {
		return extents{}, fmt.Errorf("read the index directory: %w", err)
	}
	segs := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), segDirPrefix) {
			segs++
		}
	}
	if segs != 1 {
		return extents{}, fmt.Errorf("%s holds %d segments and the working set below is derived as one contiguous "+
			"docs section, which is exact only for one: rebuild with `weft-eval build` rather than publish a figure "+
			"that reads low", path, segs)
	}

	n := ix.Len()
	e := extents{off: make([]int64, n), size: make([]int64, n)}
	var at int64
	for i := range n {
		d, ok := ix.Doc(engine.DocID(i))
		if !ok {
			return extents{}, fmt.Errorf("document %d does not read back; the index is damaged (run `weft-eval build`)", i)
		}
		sz := strLen(len(d.Key)) + strLen(len(d.Text)) +
			uvLen(uint64(ix.DocLen(engine.DocID(i)))) +
			uvLen(uint64(len(d.Vector))) + int64(4*len(d.Vector)) +
			uvLen(uint64(len(d.Links)))
		for _, l := range d.Links {
			sz += strLen(len(l))
		}
		sz += vLen(d.Time.Unix()) + uvLen(uint64(d.Time.Nanosecond()))
		sz += 4 // the record's own seeded checksum
		e.off[i], e.size[i] = at, sz
		at += sz
	}
	e.total = at
	return e, nil
}

func strLen(n int) int64 { return uvLen(uint64(n)) + int64(n) }

func uvLen(v uint64) int64 {
	n := int64(1)
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

func vLen(v int64) int64 {
	u := uint64(v) << 1
	if v < 0 {
		u = ^u
	}
	return uvLen(u)
}

// workingSet is how many bytes of records a candidate list reaches, and how many
// distinct pages those records sit on.
//
// The two are different numbers and the gap between them is the finding. A
// candidate's record is a couple of kilobytes and the candidates are scattered —
// an inverted list's members are not adjacent in the docs file, because that file
// is in DocID order and the list is in centroid order — so each one drags in at
// least one whole page whether or not it fills it. Page-granularity rounding, not
// vector bytes, is what a paging system actually charges.
func workingSet(e extents, ids []engine.DocID) (bytes, pages int) {
	seen := make(map[int64]struct{}, len(ids)*2)
	for _, id := range ids {
		i := int(id)
		if i < 0 || i >= len(e.off) {
			continue
		}
		bytes += int(e.size[i])
		first := e.off[i] / recallPageSize
		last := (e.off[i] + e.size[i] - 1) / recallPageSize
		for p := first; p <= last; p++ {
			seen[p] = struct{}{}
		}
	}
	return bytes, len(seen)
}
