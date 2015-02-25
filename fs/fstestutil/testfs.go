package fstestutil

import (
	"os"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

// SimpleFS is a trivial FS that just implements the Root method.
type SimpleFS struct {
	Node fs.Node
}

var _ = fs.FS(SimpleFS{})

func (f SimpleFS) Root() (fs.Node, error) {
	return f.Node, nil
}

// File can be embedded in a struct to make it look like a file.
type File struct{}

func (f File) Attr() fuse.Attr { return fuse.Attr{Mode: 0666} }

// Dir can be embedded in a struct to make it look like a directory.
type Dir struct{}

func (f Dir) Attr() fuse.Attr { return fuse.Attr{Mode: os.ModeDir | 0777} }
