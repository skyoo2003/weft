// The gRPC surface lives in its own module so weft keeps its stated operational
// metric: `go list -m all` in the repository root prints one line. See README.md.
//
// Same argument as bench/, and the same shape. grpc-go pulls protobuf, x/net and
// genproto behind it; none of that belongs in a module whose founding claim is
// the standard library and nothing else, and none of it is needed to *use* weft.
module github.com/skyoo2003/weft/grpc

go 1.26

require (
	github.com/skyoo2003/weft v0.0.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)

// Not a published version: the engine being shared is the point, and a tagged
// dependency would let the two surfaces drift apart between tags.
replace github.com/skyoo2003/weft => ../
