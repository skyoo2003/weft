// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/skyoo2003/weft/pkg/engine"
)

// This file is weft's own search surface, and it exists because the
// compatibility surface is somebody else's language.
//
// Everything under `_search` is weft answering a question OpenSearch knows how to
// ask. Two things weft does are questions OpenSearch has no way to ask at all:
//
//  1. **A request that is a stream list.** `hybrid` is a wrapper around one and
//     `weights` was weft's extension to that wrapper (D-030). Here the list is
//     the query and a weight is an index into a list the client wrote — which is
//     what a positional weight always was, arriving as the shape of the protocol
//     rather than as a field inside a clause.
//  2. **The pre-fusion rank of every stream, per hit.** `examples/breakdown`
//     prints it and `weft search -breakdown` prints it; an OpenSearch response
//     has nowhere to put it, so a client on the compatibility surface can see
//     that a document was ranked and never why.
//
// And one thing that is neither, but is the same disagreement: **depth is not
// size.** On `_search` a knn clause's `k` raises the candidate depth of every
// stream at once, and a text-only query has no way to ask for a deeper fusion
// than the page it wants. Here they are two numbers because they were always two
// ideas.
//
// # What it deliberately is not
//
// It is not a second engine and not a second parser. A stream is compiled by
// `compiler.clause` — the same function `_search` compiles a leaf with — so all
// eleven leaf kinds are streams here on the day they land there, and every
// refusal in PRD section 4 is inherited rather than re-listed. The route is
// `/{index}/_weft/search` for D-033's reason: what OpenSearch has no name for
// gets a name OpenSearch does not use. It cannot collide with an index either,
// because validName rejects a leading underscore.

// NativeRequest is a search in weft's own terms.
//
// Exported, along with NativeResult and NativeHit, because HTTP is not the only
// encoding of it: `grpc/` is a second module that converts protobuf to this
// struct and calls NativeSearch, so the two surfaces cannot disagree about what a
// request means without one of them failing to compile. The struct tags are
// HTTP's; the field names are the contract, and a test in that module fails the
// build if a field here has no counterpart in the .proto.
//
// Streams is []map[string]json.RawMessage and not a named union on purpose: an
// entry is exactly one leaf clause, in the spelling `_search` already accepts, so
// a client that knows one surface moves a clause to the other by cutting and
// pasting it. Naming the kinds here would be a second list to keep in step with
// `compiler.clause`, and the two would drift on the first signal added to one.
type NativeRequest struct {
	Streams []map[string]json.RawMessage `json:"streams"`
	Weights []float64                    `json:"weights"`

	Size  *int `json:"size"`
	From  *int `json:"from"`
	Depth *int `json:"depth"`

	Breakdown bool `json:"breakdown"`
}

// NativeHit is one result, in the shape both encodings build their own from.
//
// Breakdown holds one entry per stream the request named, nil where that stream
// had no opinion. HTTP writes it as JSON null; the gRPC service writes a Rank
// with present=false, because proto3 has no null and a zero would read as rank 0.
type NativeHit struct {
	ID        string
	Score     float64
	Source    json.RawMessage
	Breakdown []*int
}

// NativeResult is one page of a ranking.
//
// Total is how many candidates the fusion produced before the page was cut, and
// it is a lower bound unless Exact — the search returns a top-k, so a full prefix
// says only that at least this many matched.
type NativeResult struct {
	Hits  []NativeHit
	Total int
	Exact bool
}

// NativeSearch fuses the streams a request names and returns one page of the
// ranking.
//
// This is the whole of the native surface. Both encodings are conversion either
// side of this call and neither decides anything — which is what makes "one
// engine, two wires" a property of the code rather than a claim about it.
func NativeSearch(ctx context.Context, x *Index, req NativeRequest) (NativeResult, error) {
	if len(req.Streams) == 0 {
		return NativeResult{}, badRequest(kindIllegalArgument,
			"a native search names no streams: the request *is* the stream list, so an empty one is not an "+
				`empty ranking but a body that forgot its query. Send {"streams":[{"match":{"text":"..."}}]}`)
	}

	p := plan{size: defaultSize}
	if apiErr := readNativeWindow(&p, req); apiErr != nil {
		return NativeResult{}, apiErr
	}

	c := &compiler{ix: x.Engine(), m: x.Mapping(), p: &p}
	groups, apiErr := c.streams(req.Streams, req.Weights, "stream")
	if apiErr != nil {
		return NativeResult{}, apiErr
	}

	// The fuser is wrapped rather than the streams re-derived. Calling every
	// scorer a second time to find out where it put a document would be a second
	// opinion about a ranking that has already happened, and the two can disagree
	// the moment a scorer is not deterministic — which is exactly what a
	// breakdown exists to rule out. cmd/weft's -breakdown does it this way for
	// the same reason.
	ranks := map[engine.DocID][]int{}
	if req.Breakdown {
		p.fuse = capture(p.fuser(), ranks)
	}

	hits, total, exact, err := runSearch(ctx, x, p)
	if err != nil {
		return NativeResult{}, fmt.Errorf("native search %q: %w", x.Name(), err)
	}
	if req.Breakdown {
		attach(x, hits, ranks, groups, len(req.Streams))
	}

	out := NativeResult{Hits: make([]NativeHit, 0, len(hits)), Total: total, Exact: exact}
	for _, h := range hits {
		out.Hits = append(out.Hits, NativeHit{
			ID: h.ID, Score: h.Score, Source: h.Source, Breakdown: h.Breakdown,
		})
	}
	return out, nil
}

// nativeSearch answers POST /{index}/_weft/search.
func (s *Server) nativeSearch(w http.ResponseWriter, r *http.Request) error {
	start := time.Now()

	x, err := s.index(r)
	if err != nil {
		return err
	}
	body, err := readAll(r)
	if err != nil {
		return err
	}

	var req NativeRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return badRequest("parsing_exception", "the search body did not decode as JSON: %v", err)
		}
	}

	res, err := NativeSearch(r.Context(), x, req)
	if err != nil {
		return err
	}

	// Back into the shape writeHits already writes, so the two surfaces answer in
	// one dialect: the null-versus-empty-array rule and the took field go right
	// once rather than once per encoding.
	hits := make([]hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, hit{
			Index: x.Name(), ID: h.ID, Score: h.Score, Source: h.Source, Breakdown: h.Breakdown,
		})
	}
	writeHits(w, hits, res.Total, res.Exact, time.Since(start))
	return nil
}

// readNativeWindow reads size, from and depth, and refuses a window that would
// allocate the heap before a document is scored.
//
// depth gets the same ceiling from and size get, and for their reason rather than
// for symmetry: engine.NewCollector does make([]Candidate, 0, k) with the k it is
// handed, and runSearch hands it max(from+size, depth). A depth a request names is
// therefore a remote allocation primitive exactly as a size is, and it is refused
// before it is allocated rather than after.
func readNativeWindow(p *plan, req NativeRequest) *apiError {
	if req.Size != nil {
		if *req.Size < 0 {
			return badRequest(kindIllegalArgument, "size is not negative, got %d", *req.Size)
		}
		p.size = *req.Size
	}
	if req.From != nil {
		if *req.From < 0 {
			return badRequest(kindIllegalArgument, "from is not negative, got %d", *req.From)
		}
		p.from = *req.From
	}
	if p.from+p.size > maxResultWindow {
		return badRequest(kindIllegalArgument,
			"from %d plus size %d is over the result window of %d: a page here is the whole prefix fetched "+
				"and then cut, so a deep page costs what every page before it costs. This is refused rather "+
				"than allocated — narrow the query", p.from, p.size, maxResultWindow)
	}

	if req.Depth != nil {
		if *req.Depth < 1 {
			return badRequest(kindIllegalArgument,
				"depth is how many candidates the fusion produces before the page is cut, so it is at "+
					"least 1, got %d", *req.Depth)
		}
		if *req.Depth > maxResultWindow {
			return badRequest(kindIllegalArgument,
				"a depth of %d is over the result window of %d: the collector allocates for the depth it "+
					"is given, so this is refused before it is allocated rather than after",
				*req.Depth, maxResultWindow)
		}
		p.depth = *req.Depth
	}
	return nil
}

// capture fills into with where each stream placed each document, and then fuses
// as it would have anyway.
//
// The table is filled here, inside the call, rather than by keeping the slice
// for later: a Fuser is handed engine.Search's own streams and the wrappers under
// this one hand on slices of their own, so a reference held past the call is a
// reference to something another wrapper is entitled to have changed.
//
// It reads a position and nothing else. That is not a style note — a breakdown
// that knew stream 2 was a vector would be a fifth place to edit when a sixth
// signal arrives, and TestTheNativeFusionPathKnowsNoScorer is what holds it to
// positions.
func capture(inner engine.Fuser, into map[engine.DocID][]int) engine.Fuser {
	return func(streams [][]engine.Candidate, k int) []engine.Candidate {
		for i, s := range streams {
			for at, cand := range s {
				row, ok := into[cand.Doc]
				if !ok {
					row = make([]int, len(streams))
					for j := range row {
						row[j] = -1 // no opinion, which is not rank 0
					}
					into[cand.Doc] = row
				}
				row[i] = at
			}
		}
		return inner(streams, k)
	}
}

// rankOf folds one document's row of scorer ranks into what the wire carries: a
// rank per *entry the client wrote*, and null where that entry had no opinion.
//
// The fold is the whole function, and it is here because the two lists are not
// the same length. groups says which entry each scorer position came from, so a
// two-token match — two query.Glob streams under one entry — reports one column
// and not two. Without it the second entry's rank appears under the first
// entry's label, for every multi-token query, silently.
//
// The best rank wins when an entry became several streams. "Where did this
// stream put the document" has one honest answer when the stream is three token
// scorers, and it is the nearest one of them to the top: the alternatives are an
// average, which is a score by another name and this engine does not compare
// scores, or the first position, which would depend on token order.
//
// A null and not a zero, and not the stream length either. "Absent" is a third
// thing — the `-` column in examples/breakdown — and encoding it as a number
// would make a document nothing nominated look like one everything ranked first.
func rankOf(row, groups []int, entries int) []*int {
	best := make([]int, entries)
	for i := range best {
		best[i] = -1
	}
	for at, entry := range groups {
		if at >= len(row) || entry >= entries || row[at] < 0 {
			continue
		}
		if best[entry] < 0 || row[at] < best[entry] {
			best[entry] = row[at]
		}
	}

	out := make([]*int, entries)
	for i := range best {
		if best[i] < 0 {
			continue
		}
		out[i] = &best[i]
	}
	return out
}

// attach puts each hit's breakdown on it.
//
// runSearch reports a hit by key and the table is keyed by DocID, so the key is
// resolved back. That is one lookup per hit on a page already bounded by
// maxResultWindow, against the alternative of widening runSearch's return for a
// single caller.
//
// A hit with no row still gets a full row of nulls rather than no field: the
// column count is the entry count, and a short row would have a client index
// past the end of one document's breakdown and not another's.
func attach(x *Index, hits []hit, ranks map[engine.DocID][]int, groups []int, entries int) {
	ix := x.Engine()
	for i := range hits {
		id, live := ix.Resolve(hits[i].ID)
		if !live {
			hits[i].Breakdown = rankOf(nil, groups, entries)
			continue
		}
		hits[i].Breakdown = rankOf(ranks[id], groups, entries)
	}
}
