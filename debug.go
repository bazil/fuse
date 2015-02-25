package fuse

import (
	"runtime"
)

func stack() string {
	buf := make([]byte, 1024)
	return string(buf[:runtime.Stack(buf, false)])
}

// Debug is used to output debug messages and protocol traces. The
// default behaviour is to do nothing.
//
// The messages have human-friendly string representations and are
// safe to marshal to JSON.
//
// Implementations must not retain msg.
type Debugger interface {
	// Begin begins a trace span, returns a key to close that span.
	Begin(msg interface{}) interface{}

	// End ends a trace span.
	End(key, msg interface{})

	// Print outputs a message not from a span.
	Print(msg interface{})
}

type nopDebugger struct{}

func (nopDebugger) Begin(msg interface{}) interface{} {
	return nil
}
func (nopDebugger) End(key, msg interface{}) {}
func (nopDebugger) Print(msg interface{}) {}

var Debug Debugger = nopDebugger{}
