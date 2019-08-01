bazil.org/fuse -- Filesystems in Go
===================================

`bazil.org/fuse` is a Go library for writing FUSE userspace
filesystems.

It is a from-scratch implementation of the kernel-userspace
communication protocol, and does not use the C library from the
project called FUSE. `bazil.org/fuse` embraces Go fully for safety and
ease of programming.

Here’s how to get going:

    go get bazil.org/fuse

Website: http://bazil.org/fuse/

Github repository: https://github.com/bazil/fuse

API docs: http://godoc.org/bazil.org/fuse

Our thanks to Russ Cox for his fuse library, which this project is
based on.

# Quick Start #

Recipe using Go Modules (requires Go 1.12 or higher).

    git clone git clone https://github.com/bazil/fuse

Build and install 'hellofs':

    cd fuse
    go install ./examples/hellofs

Mount /tmp/hello:

    mkdir /tmp/hello
    hellofs /tmp/hello

Access /tmp/hello from another session:

    $ ls -al /tmp/hello
    total 0
    -r--r--r-- 1 root root 13 Jul 31 22:21 hello

    $ cat /tmp/hello/hello
    hello, world

