package fuse

import (
	"fmt"
	"os"
	"unsafe"

	"bazil.org/fuse/syscallx"
)

const fuseBufMaxSize = (1024 * 4096)

func mount(dir string, conf *mountConfig, ready chan<- struct{}, errp *error) (*os.File, error) {
	defer close(ready)

	fmt.Printf("mount(%v, %v)\n", dir, conf)
	f, err := os.OpenFile("/dev/fuse0", os.O_RDWR, 0000)
	if err != nil {
		*errp = err
		return nil, err
	}

	fuse_args := syscallx.Fusefs_args{
		FD:      int(f.Fd()),
		MaxRead: fuseBufMaxSize,
	}

	fmt.Printf("fusefs_args(%p): %#v\n", &fusefs_args, fusefs_args)

	err = syscallx.Mount("fuse", dir, 0, uintptr(unsafe.Pointer(&fuse_args)))
	return f, err
}
