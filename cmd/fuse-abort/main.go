// +build linux

// Forcibly abort a FUSE filesystem mounted at the given path.
//
// This is only supported on Linux.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/cmd/fuse-abort/internal/mountinfo"
)

// When developing a FUSE filesystem, it's pretty common to end up
// with broken mount points, where the FUSE server process is either
// no longer running, or is not responsive.
//
// The usual `fusermount -u` / `umount` commands do things like stat
// the mountpoint, causing filesystem requests. A hung filesystem
// won't answer them.
//
// The way out of this conundrum is to sever the kernel FUSE
// connection. This process is woefully underdocumented, but basically
// we need to find a "connection identifier" and then use `sysfs` to
// tell the FUSE kernelspace to abort the connection.
//
// The special sauce is knowing that the minor number of a device node
// for the mountpoint is this identifier. That and some careful
// parsing of a file listing all the mounts.
//
// https://www.kernel.org/doc/Documentation/filesystems/fuse.txt
// https://sourceforge.net/p/fuse/mailman/message/31426925/

// findFUSEMounts returns a mapping of all the known mounts in the
// current namespace. For FUSE mounts, the value will be the
// connection ID. Non-FUSE mounts store an empty string, to
// differentiate error messages.
func findFUSEMounts() (map[string]string, error) {
	r := map[string]string{}

	mounts, err := mountinfo.Open(mountinfo.DefaultPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open mountinfo: %v", err)
	}
	defer mounts.Close()
	for {
		info, err := mounts.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing mountinfo: %v", err)
		}

		if info.FSType != "fuse" && !strings.HasPrefix(info.FSType, "fuse.") {
			r[info.Mountpoint] = ""
			continue
		}
		if info.Major != "0" {
			return nil, fmt.Errorf("FUSE mount has weird device major number: %v:%v: %v", info.Major, info.Minor, info.Mountpoint)
		}
		if _, ok := r[info.Mountpoint]; ok {
			return nil, fmt.Errorf("mountpoint seen seen twice in mountinfo: %v", info.Mountpoint)
		}
		r[info.Mountpoint] = info.Minor
	}
	return r, nil
}

func abort(id string) error {
	p := filepath.Join("/sys/fs/fuse/connections", id, "abort")
	f, err := os.OpenFile(p, os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrNotExist) {
		// nothing to abort, consider that a success because we might
		// have just raced against an unmount
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("1\n"); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	f = nil
	return nil
}

func pruneEmptyDir(p string) error {
	// we want an rmdir and not a generic delete like
	// os.Remove; the node underlying the mountpoint might not
	// be a directory, and we really want to only prune
	// directories
	if err := syscall.Rmdir(p); err != nil {
		switch err {
		case syscall.ENOTEMPTY, syscall.ENOTDIR:
			// underlying node wasn't an empty dir; ignore
		case syscall.ENOENT:
			// someone else removed it for us; ignore
		default:
			err = &os.PathError{
				Op:   "rmdir",
				Path: p,
				Err:  err,
			}
			return err
		}
	}
	return nil
}

var errWarnings = errors.New("encountered warnings")

func run(prune bool, mountpoints []string) error {
	success := true
	// make an explicit effort to process mountpoints in command line
	// order, even if mountinfo is not in that order
	mounts, err := findFUSEMounts()
	if err != nil {
		return err
	}
	for _, mountpoint := range mountpoints {
		p, err := filepath.Abs(mountpoint)
		if err != nil {
			log.Printf("cannot make path absolute: %s: %v", mountpoint, err)
			success = false
			continue
		}
		id, ok := mounts[p]
		if !ok {
			log.Printf("mountpoint not found: %v", p)
			success = false
			continue
		}
		if id == "" {
			log.Printf("not a FUSE mount: %v", p)
			success = false
			continue
		}
		if err := abort(id); err != nil {
			return fmt.Errorf("cannot abort: %v is connection %v: %v", p, id, err)
		}
		if err := fuse.Unmount(p); err != nil {
			log.Printf("cannot unmount: %v", err)
			success = false
			continue
		}
		if prune {
			if err := pruneEmptyDir(p); err != nil {
				log.Printf("cannot prune mountpoint: %v", err)
				success = false
			}
		}
	}

	if !success {
		return errWarnings
	}
	return nil
}

var prog = filepath.Base(os.Args[0])

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", prog)
	fmt.Fprintf(flag.CommandLine.Output(), "  %s MOUNTPOINT..\n", prog)
	fmt.Fprintf(flag.CommandLine.Output(), "\n")
	fmt.Fprintf(flag.CommandLine.Output(), "Forcibly aborts a FUSE filesystem mounted at the given path.\n")
	fmt.Fprintf(flag.CommandLine.Output(), "\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix(prog + ": ")

	var prune bool
	flag.BoolVar(&prune, "p", false, "prune empty mountpoints after unmounting")

	flag.Usage = usage
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	if err := run(prune, flag.Args()); err != nil {
		if err == errWarnings {
			// they've already been logged
			os.Exit(1)
		}
		log.Fatal(err)
	}
}
