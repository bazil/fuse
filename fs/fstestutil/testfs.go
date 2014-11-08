package fstestutil

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

// SimpleFS is a trivial FS that just implements the Root method.
type SimpleFS struct {
	Node fs.Node
}

var _ = fs.FS(SimpleFS{})

func (f SimpleFS) Root() (fs.Node, fuse.Error) {
	return f.Node, nil
}
