package syscallx

import "fmt"

/* This is the source file for syscallx_darwin_*.go, to regenerate run

   ./generate

*/

//sys mount(tpe string, dir string, flags int, data uintptr) (err error)

func Mount(tpe string, dir string, flags int, data uintptr) (err error) {
	fmt.Printf("mount(%v, %v, %v, %v)\n", tpe, dir, flags, data)
	return mount(tpe, dir, flags, data)
}
