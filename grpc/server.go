// SPDX-License-Identifier: Apache-2.0

// Package weftgrpc is weft's native search surface in a second encoding.
//
// It lives in a module of its own, and that is the whole reason this directory
// exists rather than a file under cmd/. grpc-go brings protobuf, x/net and
// genproto with it; the root module's founding claim is the standard library and
// nothing else, checked on every run by `make deps`. bench/ quarantines bleve for
// the same reason and in the same shape — a nested module with a `replace ../`,
// invisible to `go list -m all` at the root.
//
// # It decides nothing
//
// Every method here is conversion either side of one call into
// opensearch.NativeSearch — the same function POST /{index}/_weft/search calls.
// No ranking, no refusal and no default is written twice. That is what makes
// "one engine, two wires" a property of the code instead of a claim about it,
// and TestTheTwoSurfacesAreOneEngine is what reads it back.
//
// # Read this before exposing it
//
// The warning weftd prints applies here unchanged: weft is not usable in
// production, there is no authentication and no TLS, and this server binds
// loopback by default for that reason. docs/STATUS.md and docs/LIMITATIONS.md are
// the account.
package weftgrpc

import (
	"context"
	"encoding/json"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/skyoo2003/weft/grpc/weftpb"
	"github.com/skyoo2003/weft/internal/opensearch"
)

// Service answers weftpb.Weft over a registry of open indexes.
type Service struct {
	weftpb.UnimplementedWeftServer
	reg *opensearch.Registry
}

// New wires the service to a registry.
//
// The registry may be the same one an opensearch.Server is holding: reads do not
// go through the writer goroutine, so the two surfaces serve the same indexes
// concurrently without either knowing the other is there.
func New(reg *opensearch.Registry) *Service { return &Service{reg: reg} }

// Search fuses the streams a request names and returns one page of the ranking.
func (s *Service) Search(ctx context.Context, req *weftpb.SearchRequest) (*weftpb.SearchResponse, error) {
	x, ok := s.reg.Get(req.GetIndex())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no such index [%s]", req.GetIndex())
	}

	native, err := decode(req)
	if err != nil {
		return nil, err
	}
	res, err := opensearch.NativeSearch(ctx, x, native)
	if err != nil {
		return nil, wireError(err)
	}

	out := &weftpb.SearchResponse{
		Hits:  make([]*weftpb.Hit, 0, len(res.Hits)),
		Total: int64(res.Total),
		Exact: res.Exact,
	}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, &weftpb.Hit{
			Id: h.ID, Score: h.Score, Source: h.Source, Breakdown: ranks(h.Breakdown),
		})
	}
	return out, nil
}

// decode turns the wire request into the one shape the engine reads.
//
// A stream arrives as JSON because that is what a stream is on both surfaces —
// weft.proto's field comment is why it is not a oneof. Decoding it here rather
// than handing the bytes on means a malformed clause is refused with the position
// of the entry that was wrong, which the engine could not have said.
func decode(req *weftpb.SearchRequest) (opensearch.NativeRequest, error) {
	out := opensearch.NativeRequest{
		Weights:   req.GetWeights(),
		Breakdown: req.GetBreakdown(),
	}
	for i, raw := range req.GetStreams() {
		var one map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &one); err != nil {
			return out, status.Errorf(codes.InvalidArgument,
				"stream %d did not decode as JSON: %v. A stream is one leaf clause, "+
					`spelled as _search spells it: {"match":{"text":"..."}}`, i, err)
		}
		out.Streams = append(out.Streams, one)
	}

	// Zero means "not set" on the wire and nil means it here, because proto3
	// scalars have no presence and the engine's defaults are not zero: size 0
	// would be an empty page rather than the default of ten, and depth 0 would be
	// refused rather than read as from+size.
	out.Size = optional(req.GetSize())
	out.From = optional(req.GetFrom())
	out.Depth = optional(req.GetDepth())
	return out, nil
}

func optional(v int32) *int {
	if v == 0 {
		return nil
	}
	n := int(v)
	return &n
}

// ranks converts a breakdown row for the wire.
//
// nil becomes present=false rather than 0. proto3 has no null, so the absence a
// JSON breakdown writes as `null` needs a field of its own — without it a
// document no stream nominated would arrive looking like one every stream ranked
// first, which is the one reading of a breakdown worse than not having one.
func ranks(row []*int) []*weftpb.Rank {
	if row == nil {
		return nil
	}
	out := make([]*weftpb.Rank, 0, len(row))
	for _, at := range row {
		if at == nil {
			out = append(out, &weftpb.Rank{})
			continue
		}
		//nolint:gosec // a rank is a position within a page, bounded by maxResultWindow
		out = append(out, &weftpb.Rank{Present: true, At: int32(*at)})
	}
	return out
}

// wireError maps the engine's refusal onto a gRPC code.
//
// The classification is opensearch.StatusOf's and not a second opinion, so a
// request that is a 400 over HTTP is InvalidArgument over gRPC rather than
// Unknown. That mapping is the whole of D-026 on this wire: a query this engine
// cannot express arrives as a code the client can branch on, never as an empty
// result list.
func wireError(err error) error {
	httpStatus, _ := opensearch.StatusOf(err)
	code := codes.Internal
	switch httpStatus {
	case http.StatusBadRequest:
		code = codes.InvalidArgument
	case http.StatusNotFound:
		code = codes.NotFound
	case http.StatusNotImplemented:
		code = codes.Unimplemented
	case http.StatusServiceUnavailable:
		code = codes.Unavailable
	case http.StatusRequestEntityTooLarge:
		code = codes.ResourceExhausted
	}
	return status.Error(code, err.Error())
}
