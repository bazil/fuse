package fstestutil

import (
	"syscall"
	"testing"
	"time"
)

func waitForMount(t testing.TB, dir string, abort <-chan struct{}) {
	for tries := 0; tries < 100; tries++ {
		var st syscall.Statfs_t
		err := syscall.Statfs(dir, &st)
		if err == nil {
			// args Fstypename is a string stored in [16]int8, no easy
			// conversion
			var typ string
			for _, c := range st.Fstypename {
				if c == 0 {
					break
				}
				typ = typ + string(byte(c))
			}

			// TODO this is false positive if the temp dir was on
			// fuse already.
			if typ == "osxfusefs" {
				return
			}
			t.Logf("waiting for root: %q", typ)
		}
		select {
		case <-abort:
			t.Logf("waiting aborted")
			return
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("mount did not work")
}
