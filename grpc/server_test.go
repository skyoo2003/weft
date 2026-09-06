// SPDX-License-Identifier: Apache-2.0

package weftgrpc

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/skyoo2003/weft/grpc/weftpb"
	"github.com/skyoo2003/weft/internal/opensearch"
)

// Milestone 31. The same surface, a second encoding.
//
// What this module has to prove is not that gRPC works — it is somebody else's
// well-tested library. It is three things about *this* repository:
//
//  1. The root module still prints one line for `go list -m all`. That is
//     `make deps`, not a test in here, and it is the reason this directory is a
//     module of its own — the same argument bench/ makes about bleve.
//  2. The two surfaces are one engine. A request sent both ways ranks the same
//     documents, because the gRPC service decides nothing and calls what the
//     HTTP handler calls.
//  3. They cannot drift. A field on one side and not the other fails the build,
//     which is TestTheProtoAndTheCoreDoNotDrift below.

// corpus builds an index and returns the registry it lives in, plus an HTTP
// server on the same registry.
//
// Documents go in over HTTP because that is the surface that has a writer: this
// module deliberately exposes only search, and reaching engine.Document from here
// would mean exporting the mapping-aware constructor that turns a JSON body into
// one. Indexing one way and searching the other is also the point of the
// comparison below.
func corpus(t *testing.T) (*opensearch.Registry, *httptest.Server) {
	t.Helper()

	reg, err := opensearch.OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	t.Cleanup(func() { reg.Close() }) //nolint:errcheck // the test is over

	hsrv := httptest.NewServer(opensearch.NewServer(reg, 1<<20))
	t.Cleanup(hsrv.Close)

	call(t, hsrv, http.MethodPut, "/papers",
		`{"mappings":{"properties":{"vec":{"type":"knn_vector","dimension":3}}}}`, http.StatusOK)
	for _, d := range []struct{ id, body string }{
		{"rrf", `{"text":"reciprocal rank fusion combines rankings","vec":[1,0,0]}`},
		{"bm25", `{"text":"bm25 ranks documents by term frequency","vec":[0.9,0.1,0]}`},
		{"pottery", `{"text":"an unrelated document about pottery","vec":[0,1,0]}`},
	} {
		call(t, hsrv, http.MethodPut, "/papers/_doc/"+d.id, d.body, http.StatusCreated)
	}
	return reg, hsrv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, want int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read to completion above
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d (body %s)", method, path, resp.StatusCode, want, raw)
	}
	return raw
}

// dial starts the service on a loopback port and returns a client for it.
//
// A real socket rather than bufconn: what is being checked is that a client
// speaking gRPC over TCP reaches this engine, and bufconn would skip the half of
// that which can actually be misconfigured.
func dial(t *testing.T, reg *opensearch.Registry) weftpb.WeftClient {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	weftpb.RegisterWeftServer(srv, New(reg))
	go srv.Serve(ln) //nolint:errcheck // Stop below ends this
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck // the test is over
	return weftpb.NewWeftClient(conn)
}

func hitIDs(hits []*weftpb.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.GetId())
	}
	return out
}

// ------------------------------------------------------ one engine, two wires

// The judgment sentence of this milestone that a test can hold: the same request,
// sent over HTTP and over gRPC, ranks the same documents in the same order.
//
// Not "returns results". The order is the assertion, because a second encoding
// that fused differently would still return results.
func TestTheTwoSurfacesAreOneEngine(t *testing.T) {
	reg, hsrv := corpus(t)
	client := dial(t, reg)

	streams := []string{
		`{"match":{"text":"rank fusion"}}`,
		`{"knn":{"vec":{"vector":[1,0,0],"k":2}}}`,
	}

	res, err := client.Search(t.Context(), &weftpb.SearchRequest{
		Index: "papers", Streams: streams, Weights: []float64{1, 0.5}, Size: 5,
	})
	if err != nil {
		t.Fatalf("Search over gRPC: %v", err)
	}

	raw := call(t, hsrv, http.MethodPost, "/papers/_weft/search",
		`{"streams":[`+strings.Join(streams, ",")+`],"weights":[1,0.5],"size":5}`, http.StatusOK)
	var overHTTP struct {
		Hits struct {
			Hits []struct {
				ID string `json:"_id"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &overHTTP); err != nil {
		t.Fatalf("decode the HTTP answer: %v", err)
	}
	want := make([]string, 0, len(overHTTP.Hits.Hits))
	for _, h := range overHTTP.Hits.Hits {
		want = append(want, h.ID)
	}

	if got := hitIDs(res.GetHits()); !slices.Equal(got, want) {
		t.Errorf("gRPC ranked %v and HTTP ranked %v: the two encodings are supposed to be one engine",
			got, want)
	}
}

// The document body survives the second encoding. It is `bytes` on the wire
// rather than a string, because it is JSON weft stored verbatim and re-encoding
// it would be this service having an opinion about somebody else's document.
func TestTheSourceSurvivesTheWire(t *testing.T) {
	reg, _ := corpus(t)
	client := dial(t, reg)

	res, err := client.Search(t.Context(), &weftpb.SearchRequest{
		Index: "papers", Streams: []string{`{"match":{"text":"pottery"}}`}, Size: 1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.GetHits()) != 1 {
		t.Fatalf("got %d hits, want 1", len(res.GetHits()))
	}

	var body map[string]any
	if err := json.Unmarshal(res.GetHits()[0].GetSource(), &body); err != nil {
		t.Fatalf("the source did not decode as JSON: %v", err)
	}
	if body["text"] != "an unrelated document about pottery" {
		t.Errorf("source came back as %v", body)
	}
}

// A breakdown crosses the wire with "absent" intact.
//
// proto3 has no null, so Rank is a message: present=false is the `-` column in
// examples/breakdown, and a document no stream nominated must not arrive looking
// like one every stream ranked first.
func TestBreakdownCrossesTheWireWithAbsenceIntact(t *testing.T) {
	reg, _ := corpus(t)
	client := dial(t, reg)

	res, err := client.Search(t.Context(), &weftpb.SearchRequest{
		Index: "papers",
		Streams: []string{
			`{"match":{"text":"pottery"}}`,
			`{"match":{"text":"fusion"}}`,
		},
		Size: 5, Breakdown: true,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	seen := map[string][]*weftpb.Rank{}
	for _, h := range res.GetHits() {
		seen[h.GetId()] = h.GetBreakdown()
	}
	for id, want := range map[string][2]bool{
		"pottery": {true, false},
		"rrf":     {false, true},
	} {
		got, ok := seen[id]
		if !ok {
			t.Errorf("%q is not among %v", id, hitIDs(res.GetHits()))
			continue
		}
		if len(got) != 2 {
			t.Errorf("%q has %d breakdown columns, want one per stream (2)", id, len(got))
			continue
		}
		for i, present := range want {
			if got[i].GetPresent() != present {
				t.Errorf("%q stream %d: present=%v, want %v", id, i, got[i].GetPresent(), present)
			}
		}
	}
}

// A response nobody asked a breakdown of carries none, rather than a column of
// present=false. The wire says "not asked" by being empty.
func TestBreakdownIsAbsentUnlessAsked(t *testing.T) {
	reg, _ := corpus(t)
	client := dial(t, reg)

	res, err := client.Search(t.Context(), &weftpb.SearchRequest{
		Index: "papers", Streams: []string{`{"match":{"text":"pottery"}}`}, Size: 5,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range res.GetHits() {
		if len(h.GetBreakdown()) != 0 {
			t.Errorf("hit %q carries a breakdown nobody asked for", h.GetId())
		}
	}
}

// ------------------------------------------------------------------ refusals

// D-026's rule reaching a second protocol: a request this engine cannot answer
// gets a status code and a message, never an empty result list.
//
// The HTTP statuses map onto gRPC codes rather than being flattened to Unknown,
// because a client branches on the code and "unknown" is not a branch.
func TestRefusalsArriveAsStatusCodes(t *testing.T) {
	reg, _ := corpus(t)
	client := dial(t, reg)

	for _, tc := range []struct {
		name string
		req  *weftpb.SearchRequest
		want codes.Code
		says string
	}{
		{"no such index",
			&weftpb.SearchRequest{Index: "nope", Streams: []string{`{"match":{"text":"a"}}`}},
			codes.NotFound, "nope"},
		{"no streams",
			&weftpb.SearchRequest{Index: "papers"},
			codes.InvalidArgument, "stream"},
		{"unknown stream kind",
			&weftpb.SearchRequest{Index: "papers", Streams: []string{`{"mach":{"text":"a"}}`}},
			codes.InvalidArgument, "mach"},
		{"a stream that is not JSON",
			&weftpb.SearchRequest{Index: "papers", Streams: []string{`not json`}},
			codes.InvalidArgument, "JSON"},
		{"weights that do not match",
			&weftpb.SearchRequest{Index: "papers", Streams: []string{`{"match":{"text":"a"}}`},
				Weights: []float64{1, 1}},
			codes.InvalidArgument, "positional"},
		{"a depth over the window",
			&weftpb.SearchRequest{Index: "papers", Streams: []string{`{"match":{"text":"a"}}`},
				Depth: 2000000000},
			codes.InvalidArgument, "10000"},
		{"a query this engine cannot express",
			&weftpb.SearchRequest{Index: "papers", Streams: []string{`{"query_string":{"query":"a"}}`}},
			codes.Unimplemented, "query string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Search(t.Context(), tc.req)
			if err == nil {
				t.Fatalf("%s answered without an error; a refusal that becomes an empty result is the "+
					"failure D-026 exists to prevent", tc.name)
			}
			st, _ := status.FromError(err)
			if st.Code() != tc.want {
				t.Errorf("%s: code %s, want %s (message %q)", tc.name, st.Code(), tc.want, st.Message())
			}
			if !strings.Contains(st.Message(), tc.says) {
				t.Errorf("%s: message %q does not mention %q", tc.name, st.Message(), tc.says)
			}
		})
	}
}

// ----------------------------------------------------------------- the drift

// TestTheProtoAndTheCoreDoNotDrift is this module's reason to exist as code
// rather than as a README.
//
// Two surfaces over one engine is only true while both carry the same request. A
// field added to opensearch.NativeRequest and not to SearchRequest is a feature
// that silently does not exist over gRPC — no error, no warning, just a client
// setting something nobody reads. This fails the build instead.
//
// Matched by name, Go's exported spelling to snake_case. `index` is the one proto
// field with no Go counterpart, and the reason is structural rather than an
// oversight: on HTTP the index is in the path, so a request struct decoded from a
// body has nowhere to put it.
func TestTheProtoAndTheCoreDoNotDrift(t *testing.T) {
	for _, pair := range []struct {
		core  any
		msg   protoreflect.MessageDescriptor
		extra []string // proto fields with a recorded reason for having no Go field
	}{
		{opensearch.NativeRequest{}, (&weftpb.SearchRequest{}).ProtoReflect().Descriptor(), []string{"index"}},
		{opensearch.NativeResult{}, (&weftpb.SearchResponse{}).ProtoReflect().Descriptor(), nil},
		{opensearch.NativeHit{}, (&weftpb.Hit{}).ProtoReflect().Descriptor(), nil},
	} {
		rt := reflect.TypeOf(pair.core)
		t.Run(rt.Name(), func(t *testing.T) {
			inProto := map[string]bool{}
			fields := pair.msg.Fields()
			for i := range fields.Len() {
				inProto[string(fields.Get(i).Name())] = true
			}

			for i := range rt.NumField() {
				name := snake(rt.Field(i).Name)
				if !inProto[name] {
					t.Errorf("%s.%s has no %q field in %s: it would be a request field no gRPC client "+
						"can set and no gRPC server can read",
						rt.Name(), rt.Field(i).Name, name, pair.msg.FullName())
				}
				delete(inProto, name)
			}
			for name := range inProto {
				if slices.Contains(pair.extra, name) {
					continue
				}
				t.Errorf("%s has a %q field with nothing behind it in %s: a client setting it would be "+
					"heard by nobody", pair.msg.FullName(), name, rt.Name())
			}
		})
	}
}

// snake turns an exported Go field name into the proto spelling of it.
func snake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
