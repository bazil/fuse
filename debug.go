package fuse

import (
	"runtime"

	"golang.org/x/net/context"
)

func stack() string {
	buf := make([]byte, 1024)
	return string(buf[:runtime.Stack(buf, false)])
}

func nop(ctx context.Context, msg interface{}) {}

// Debug is called to output debug messages, including protocol
// traces. The default behavior is to do nothing.
//
// The messages have human-friendly string representations and are
// safe to marshal to JSON.
//
// Implementations must not retain msg.
var Debug func(ctx context.Context, msg interface{}) = nop
