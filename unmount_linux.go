package fuse

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
)

func unmount(dir string) error {
	var cmd *exec.Cmd
	// Don't use fusermount when running as root
	// (This is primarily of interest for running the tests under travis.)
	if os.Getuid() == 0 {
		cmd = exec.Command("umount", dir)
	} else {
		cmd = exec.Command("fusermount", "-u", dir)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 0 {
			output = bytes.TrimRight(output, "\n")
			msg := err.Error() + ": " + string(output)
			err = errors.New(msg)
		}
		return err
	}
	return nil
}
