package fuseutil // import "bazil.org/fuse/fuseutil"

import (
	"bazil.org/fuse"
)

// HandleRead handles a read request assuming that data is the entire file content.
// It adjusts the amount returned in resp according to req.Offset and req.Size.
func HandleRead(req *fuse.ReadRequest, resp *fuse.ReadResponse, data []byte) {
	if req.Offset >= int64(len(data)) || req.Size == 0 {
		resp.Data = resp.Data[0:0]
		return
	}
	size := req.Size
	if len(data)-int(req.Offset) < req.Size {
		size = len(data)
	}
	if len(resp.Data) < size {
		resp.Data = append(resp.Data, make([]byte, size-len(resp.Data))...)
	}
	n := copy(resp.Data[:size], data[req.Offset:])
	resp.Data = resp.Data[:n]
}
