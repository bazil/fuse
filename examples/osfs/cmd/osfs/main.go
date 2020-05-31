/*
Package main mounts an osfs on a directory.
*/
package main

import (
	"flag"
	"fmt"
	"os"

	"bazil.org/fuse"
	"bazil.org/fuse/examples/osfs"
	"bazil.org/fuse/fs"
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n  %s BASEDIR MOUNTPOINT\n", os.Args[0], os.Args[0])
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() != 2 {
		usage()
		os.Exit(2)
	}
	if err := do(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func do(baseDirPath string, mountpoint string) (retErr error) {
	if err := os.MkdirAll(mountpoint, 0777); err != nil {
		return err
	}
	conn, err := fuse.Mount(
		mountpoint,
		fuse.FSName("os"),
		fuse.Subtype("osfs"),
		fuse.VolumeName("osfs"),
		fuse.LocalVolume(),
		fuse.MaxReadahead(1<<32-1),
	)
	if err != nil {
		return err
	}
	defer func() {
		if err := conn.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	if err := fs.Serve(conn, osfs.New(baseDirPath)); err != nil {
		return err
	}
	<-conn.Ready
	return conn.MountError
}
