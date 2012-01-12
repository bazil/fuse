// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fuse

import (
	"io/ioutil"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestFuse(t *testing.T) {
	dir := "/mnt"
	exec.Command("umount", dir).Run()
	os.MkdirAll(dir, 0777)
	
	c, err := Mount(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer exec.Command("umount", dir).Run()

	go c.Serve(testFS{})
	time.Sleep(1*time.Second)
	
	_, err = os.Stat(dir+"/"+fuseTests[0].name)
	if err != nil {
		t.Fatalf("mount did not work")
		return
	}

	for _, tt := range fuseTests {
		tt.node.test(dir+"/"+tt.name, t)
	}
}

var fuseTests = []struct{
	name string
	node interface { Node; test(string, *testing.T) }
}{
	{"readAll", readAll{}},
	{"writeAll", &writeAll{}},
}

type readAll struct{ file }

const hi = "hello, world"
func (readAll) ReadAll(intr Intr) ([]byte, Error) {
	return []byte(hi), nil
}

func (readAll) test(path string, t *testing.T) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		t.Errorf("readAll: %v", err)
		return
	}
	if string(data) != hi {
		t.Errorf("readAll = %q, want %q", data, hi)
	}
}

type writeAll struct {
	file
	data []byte
}

func (w *writeAll) WriteAll(data []byte, intr Intr) Error {
	w.data = data
	return nil
}

func (w *writeAll) test(path string, t *testing.T) {
	err := ioutil.WriteFile(path, []byte(hi), 0666)
	if err != nil {
		t.Errorf("WriteFile: %v", err)
		return
	}
	if string(w.data) != hi {
		t.Errorf("writeAll = %q, want %q", w.data, hi)
	}
}

type file struct {}
type dir struct {}

func (f file) Attr() Attr { return Attr{Mode: 0666} }
func (f dir) Attr() Attr { return Attr{Mode: os.ModeDir | 0777} }

type testFS struct {}

func (testFS) Root() (Node, Error) {
	return testFS{}, nil
}

func (testFS) Attr() Attr {
	return Attr{Mode: os.ModeDir | 0555}
}

func (testFS) Lookup(name string, intr Intr) (Node, Error) {
	for _, tt := range fuseTests {
		if tt.name == name {
			return tt.node, nil
		}
	}
	return nil, ENOENT
}

func (testFS) ReadDir(intr Intr) ([]Dirent, Error) {
	var dirs []Dirent
	for _, tt := range fuseTests {
		dirs = append(dirs, Dirent{Name: tt.name})
	}
	return dirs, nil
}
