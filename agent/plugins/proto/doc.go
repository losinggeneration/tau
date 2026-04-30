// Package proto contains the gRPC plugin protocol for tau plugins.
//
// The .pb.go and _grpc.pb.go files in this directory are generated from
// plugin.proto. To regenerate, run `go generate` from this directory.
package proto

//go:generate protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative plugin.proto

// CompactionHookResult is the result of a compaction hook invocation.
// This mirrors the proto CompactionResponse for use in Go interfaces.
type CompactionHookResult struct {
	Summary          string
	FirstKeptEntryID string
}
