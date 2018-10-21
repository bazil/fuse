package fuseutil

import (
	"testing"

	"bazil.org/fuse"
)

func TestHandleRead(t *testing.T) {
	type args struct {
		req  *fuse.ReadRequest
		resp *fuse.ReadResponse
		data []byte
	}
	type validate func(args) bool
	const sampleFileData = `01234567890123456789123456789`
	const thisIsSparta = `this is sparta`
	var response = new(fuse.ReadResponse)
	tests := []struct {
		hook     func()
		name     string
		args     args
		validate validate
	}{
		{
			hook: func() { *response = fuse.ReadResponse{Data: make([]byte, len(sampleFileData))} },
			name: "Copy entire file : non-empty file",
			args: args{req: &fuse.ReadRequest{Offset: 0, Size: len(sampleFileData)},
				resp: response,
				data: []byte(sampleFileData)},
			validate: func(args args) bool { return len(args.resp.Data) == len(sampleFileData) },
		},
		{
			hook: func() { *response = fuse.ReadResponse{Data: make([]byte, len(sampleFileData))} },
			name: "Copy entire file : empty file",
			args: args{req: &fuse.ReadRequest{Offset: 0, Size: len(sampleFileData)},
				resp: response,
				data: []byte("")},
			validate: func(args args) bool { return string(args.resp.Data) == "" },
		},
		{
			name: "Copy entire file : non-empty file while response alredy have data but smaller than file",
			hook: func() { *response = fuse.ReadResponse{Data: []byte(thisIsSparta)} },
			args: args{req: &fuse.ReadRequest{Offset: 0, Size: len(sampleFileData)},
				resp: response,
				data: []byte(sampleFileData)},
			// previous implementation
			// validate: func(args args) bool { return string(args.resp.Data) == sampleFileData[:len(thisIsSparta)] },
			validate: func(args args) bool { return string(args.resp.Data) == sampleFileData },
		},
	}
	for _, tt := range tests {
		if tt.hook != nil {
			tt.hook()
		}
		HandleRead(tt.args.req, tt.args.resp, tt.args.data)
		if tt.validate(tt.args) == false {
			t.Error(tt.name)
		}
	}

}
