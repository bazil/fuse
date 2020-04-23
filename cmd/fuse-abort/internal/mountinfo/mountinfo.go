package mountinfo

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
)

const DefaultPath = "/proc/self/mountinfo"

func Open(p string) (*Reader, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	rr := &Reader{
		closer:  f,
		scanner: bufio.NewScanner(f),
	}
	return rr, nil
}

type Reader struct {
	closer  io.Closer
	scanner *bufio.Scanner
}

var _ io.Closer = (*Reader)(nil)

func (r *Reader) Close() error {
	return r.closer.Close()
}

// unescape backslash-prefixed octal
func unescape(in []byte) (string, error) {
	buf := make([]byte, 0, len(in))
	for len(in) > 0 {
		if in[0] == '\\' {
			if len(in) < 4 {
				return "", fmt.Errorf("truncated octal sequence: %q", in[1:])
			}
			octal := string(in[1:4])
			in = in[4:]
			num, err := strconv.ParseUint(octal, 8, 8)
			if err != nil {
				return "", err
			}
			buf = append(buf, byte(num))
			continue
		}

		buf = append(buf, in[0])
		in = in[1:]
	}
	return string(buf), nil
}

func (r *Reader) Next() (*Mount, error) {
	if !r.scanner.Scan() {
		err := r.scanner.Err()
		if err != nil {
			return nil, err
		}
		return nil, io.EOF
	}

	info, err := parse(r.scanner.Bytes())
	if err != nil {
		return nil, err
	}
	return info, nil
}

func parse(line []byte) (*Mount, error) {
	// https://www.kernel.org/doc/Documentation/filesystems/proc.txt
	//
	// mountinfo fields are space-separated, with octal escapes
	//
	//     id parentId maj:min path_inside mountpoint mount_options [optional fields...] - fstype /dev/sdblah super_block_options
	//
	// we care about maj:min, mountpoint, fstype

	fields := bytes.Split(line, []byte{' '})
	const minFields = 7
	if len(fields) < minFields {
		return nil, fmt.Errorf("cannot parse mountinfo entry: %q", line)
	}

	majmin := fields[2]
	idx := bytes.IndexByte(majmin, ':')
	if idx == -1 {
		return nil, fmt.Errorf("cannot parse mountinfo entry, weird major:minor: %q", line)
	}
	major := string(majmin[:idx])
	minor := string(majmin[idx+1:])

	mountpoint, err := unescape(fields[4])
	if err != nil {
		return nil, fmt.Errorf("cannot parse mountinfo entry, invalid escape in mountpoint: %q", line)
	}

	rest := fields[6:]

	// skip optional fields
	for {
		if len(rest) == 0 {
			return nil, fmt.Errorf("cannot parse mountinfo entry, no optional field terminator: %q", line)
		}
		cur := rest[0]
		rest = rest[1:]
		if bytes.Equal(cur, []byte{'-'}) {
			break
		}
	}

	if len(rest) == 0 {
		return nil, fmt.Errorf("cannot parse mountinfo entry, no fstype: %q", line)
	}
	fstype, err := unescape(rest[0])
	if err != nil {
		return nil, fmt.Errorf("cannot parse mountinfo entry, invalid escape in fstype: %q", line)
	}

	i := &Mount{
		Major:      major,
		Minor:      minor,
		Mountpoint: mountpoint,
		FSType:     fstype,
	}
	return i, nil
}

type Mount struct {
	Major      string
	Minor      string
	Mountpoint string
	FSType     string
}
