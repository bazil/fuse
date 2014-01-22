package fstestutil

import (
	"flag"
)

var debug = flag.Bool("fuse.debug", false, "log FUSE processing details")

// DebugByDefault changes the default of the `-fuse.debug` flag to
// true.
//
// This package registers a command line flag `-fuse.debug` and when
// run with that flag (and activated inside the tests), logs FUSE
// debug messages.
//
// This is disabled by default, as most callers probably won't care
// about FUSE details. Use DebugByDefault for tests where you'd
// normally be passing `-fuse.debug` all the time anyway.
//
// Call from an init function.
func DebugByDefault() {
	f := flag.Lookup("fuse.debug")
	f.DefValue = "true"
	f.Value.Set(f.DefValue)
}
