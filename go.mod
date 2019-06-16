module github.com/nestybox/sysvisor-fs/bazil

go 1.13

require (
	bazil.org/fuse v0.0.0-00010101000000-000000000000
	golang.org/x/net v0.0.0-20190613194153-d28f0bde5980
	golang.org/x/sys v0.0.0-20190614160838-b47fdc937951
)

replace bazil.org/fuse => ./
