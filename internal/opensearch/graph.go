// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/skyoo2003/weft/pkg/engine"
	"github.com/skyoo2003/weft/pkg/scorer/graph"
)

// clauseWeftGraph is not an OpenSearch query, and its name says so.
//
// D-026 confines the untruth to the version handshake: everything past it is
// honest about what this engine is. So a signal OpenSearch does not have gets a
// name OpenSearch does not use — a client sending standard DSL never reaches it
// by accident, and a client reading this back knows which half of the surface it
// is on. `hybrid.weights` is the precedent, an extension inside a query that
// exists; this is the same move with a whole clause.
const clauseWeftGraph = "weft_graph"

// graphClause is the wire form.
//
// seeds are document ids, which is engine.Query.Seeds: the escape hatch that
// keeps graph proximity usable as a signal of its own instead of a function of
// whatever the text scorer happened to find. In a Go program the alternative is
// to hand graph.New another scorer to start from; over HTTP there is no "the
// scorer written before this one", so seeds are how a walk gets a starting point
// and there is no second way.
type graphClause struct {
	Seeds        []string `json:"seeds"`
	IncludeSeeds bool     `json:"include_seeds"`

	// PPR swaps the hop-counting walk for a personalized PageRank one.
	//
	// ponytail: the adjacency is built per query, which is a pass over every
	// document's links. At the corpus sizes this server is honest about that is
	// cheap; on a large one it is the dominant cost of the query. Cache it per
	// index generation if anyone runs this at a rate.
	PPR bool `json:"ppr"`

	// Pointers, so "not given" and "given as zero" stay different. A restart of 0
	// is a walk that never restarts, which is a thing to ask for and a very
	// different thing from leaving the default alone.
	Restart   *float64 `json:"restart"`
	Precision *float64 `json:"precision"`
}

// weftGraph compiles the clause into one stream.
func (c *compiler) weftGraph(body json.RawMessage) (compiled, *apiError) {
	var cl graphClause
	if err := json.Unmarshal(body, &cl); err != nil {
		return compiled{}, badRequest("parsing_exception", "the %s clause did not decode: %v", clauseWeftGraph, err)
	}

	// The mapping first, because it is the reason a request could be well-formed
	// and still answer nothing: an index binding no field to links holds no edges
	// at all, and a walk over no edges is an empty result that looks exactly like
	// a walk which found nothing.
	if _, ok := c.m.Links(); !ok {
		return compiled{}, badRequest(kindIllegalArgument,
			"no field in this index is mapped with \"links\": true, so no document carries "+
				"engine.Document.Links and there is nothing to walk. A link is a document id and the wire "+
				"cannot tell one array of strings from another, so the mapping is what says which array "+
				"holds edges — declare it on a %s field before indexing", typeKeyword)
	}
	if len(cl.Seeds) == 0 {
		return compiled{}, badRequest(kindIllegalArgument,
			"%s needs seeds: a walk starts somewhere, and over HTTP there is no scorer written before this "+
				"one to start from. Give seeds a list of document ids", clauseWeftGraph)
	}
	// Every seed is resolved before anything is searched. A seed naming no
	// document contributes nothing to the walk, so a request whose only seed is a
	// typo would come back as a successful empty ranking.
	var missing []string
	for _, s := range cl.Seeds {
		if _, ok := c.ix.Resolve(s); !ok {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return compiled{}, badRequest(kindIllegalArgument,
			"no live document has the id %s: a seed that resolves to nothing is a walk with no starting "+
				"point, and dropping it silently would answer a different query than the one asked",
			strings.Join(quoteAll(missing), ", "))
	}
	if !cl.PPR && (cl.Restart != nil || cl.Precision != nil) {
		return compiled{}, badRequest(kindIllegalArgument,
			"restart and precision configure the personalized pagerank walk and nothing else, and this "+
				"clause did not ask for one: accepting them here would leave a number tuned and a ranking "+
				"unchanged. Set \"ppr\": true, or drop them")
	}

	// Seeds go onto the plan's Query rather than into the scorer, because that is
	// where engine.Query carries them — and because two graph streams in one
	// hybrid would otherwise each hold a private opinion of what the seeds are.
	c.p.q.Seeds = append(c.p.q.Seeds, cl.Seeds...)

	if cl.PPR {
		return c.personalized(cl)
	}
	if cl.IncludeSeeds {
		return compiled{scorers: []engine.Scorer{graph.NewIncludingSeeds(c.ix, seedless{})}}, nil
	}
	return compiled{scorers: []engine.Scorer{graph.New(c.ix, seedless{})}}, nil
}

// personalized builds the PPR stream.
func (c *compiler) personalized(cl graphClause) (compiled, *apiError) {
	adj, err := graph.NewAdjacency(context.Background(), c.ix)
	if err != nil {
		return compiled{}, badRequest(kindIllegalArgument, "reading the link graph: %v", err)
	}

	var opts []graph.Option
	if cl.Restart != nil {
		opts = append(opts, graph.WithRestart(*cl.Restart))
	}
	if cl.Precision != nil {
		opts = append(opts, graph.WithPrecision(*cl.Precision))
	}
	ppr, err := graph.NewPPR(adj, seedless{}, opts...)
	if err != nil {
		// The constructor validates the options, so its own words are what a
		// client should read: restating them here would be a second place for the
		// bounds on a restart probability to be written down.
		return compiled{}, badRequest(kindIllegalArgument, "%v", err)
	}
	return compiled{scorers: []engine.Scorer{ppr}}, nil
}

// seedless is the seed scorer a graph scorer is constructed with here, and it is
// never consulted.
//
// graph.New takes a scorer to start from and falls back to it only when
// engine.Query.Seeds is empty — and weftGraph refuses an empty seed list before
// reaching this. So the honest value to pass is one that nominates nothing: any
// real scorer here would be a second and invisible source of seeds, which a
// client never asked for and could not see in the response.
type seedless struct{}

func (seedless) Name() string { return "seedless" }

func (seedless) Candidates(context.Context, engine.Query, int) ([]engine.Candidate, error) {
	return nil, nil
}

// linksOf reads the links field of one document body.
//
// A single string is accepted beside an array because OpenSearch draws no
// distinction between a value and a one-element array of it, and a client
// writing "cites": "bm25" has said something unambiguous.
func linksOf(field string, raw json.RawMessage) ([]string, error) {
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, fmt.Errorf("%q is the links field and holds document ids, so its value is a string or an "+
			"array of strings: %w", field, err)
	}
	return []string{one}, nil
}

// quoteAll is for an error that lists values back to a client.
func quoteAll(vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, fmt.Sprintf("%q", v))
	}
	return out
}
