package main

import (
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"code.google.com/p/rsc/fuse"
)

var nhandle fuse.Handle = 2

func main() {
	c, err := fuse.Mount("/mnt/acme")
	if err != nil {
		log.Fatal(err)
	}
	
	for {
		req, err := c.ReadRequest()
		if err != nil {
			log.Fatal(err)
		}

		fmt.Printf("<- %s\n", req)
		var resp interface{}
		switch r := req.(type) {
		default:
			log.Fatal("unknown request", req)
		case *fuse.InitRequest:
			s := &fuse.InitResponse{
				Major:        7,
				Minor:        8,
				MaxReadahead: 0,
				Flags:        0,
				MaxWrite:     4096,
			}
			resp = s
			r.Respond(s)
		case *fuse.StatfsRequest:
			s := &fuse.StatfsResponse{}
			resp = s
			r.Respond(s)
		case *fuse.GetattrRequest:
			switch r.Node {
			case fuse.RootID:
				s := &fuse.GetattrResponse{
					AttrValid: 1*time.Minute,
					Attr: fuse.Attr{
						Inode: 1,
						Atime: time.Now(),
						Mtime: time.Now(),
						Ctime: time.Now(),
						Mode: os.ModeDir | 0777,
						Nlink: 1,
					},
				}
				resp = s
				r.Respond(s)
			case 2:
				s := &fuse.GetattrResponse{
					AttrValid: 1*time.Minute,
					Attr: fuse.Attr{
							Inode: 2,
							Size: 0,
							Blocks: 0,
							Mode: 0444,
							Atime: time.Now(),
							Mtime: time.Now(),
							Ctime: time.Now(),
							Crtime: time.Now(),
							Nlink: 1,
					},
				}
				resp = s
				r.Respond(s)
			default:
				log.Fatal("unexpected getattr")
			}
		case *fuse.LookupRequest:
			switch r.Node {
			case fuse.RootID:
				switch r.Name {
				case "hello":
					s := &fuse.LookupResponse{
						Node: 2,
						Generation: 1,
						EntryValid: 1*time.Minute,
						AttrValid: 1*time.Minute,
						Attr: fuse.Attr{
							Inode: 2,
							Size: 0,
							Blocks: 0,
							Mode: 0444,
							Atime: time.Now(),
							Mtime: time.Now(),
							Ctime: time.Now(),
							Crtime: time.Now(),
							Nlink: 1,
						},
					}
					r.Respond(s)
					resp = s
				default:
					r.RespondError(syscall.ENOENT)
					resp = syscall.ENOENT
				}
			}
		case *fuse.OpenRequest:
			s := &fuse.OpenResponse{
				Handle: fuse.Handle(r.Node),
				Flags: fuse.OpenDirectIO,  // ignore sizes
			}
			nhandle++
			resp = s
			r.Respond(s)
		case *fuse.ReadRequest:
			var data []byte
			switch r.Handle {
			case 1:
				data = fuse.AppendDirent(data, fuse.Dirent{Inode: 1, Name: "."})
				data = fuse.AppendDirent(data, fuse.Dirent{Inode: 1, Name: ".."})
				data = fuse.AppendDirent(data, fuse.Dirent{Inode: 2, Name: "hello", Type: 0})
			case 2:
				data = []byte("hello, world\n")
			}
			if r.Offset >= int64(len(data)) {
				data = nil
			} else {
				data = data[r.Offset:]
			}
			if len(data) > r.Size {
				data = data[:r.Size]
			}
			s := &fuse.ReadResponse{Data: data}
			resp = s
			r.Respond(s)
		case *fuse.ForgetRequest, *fuse.AccessRequest, *fuse.ReleaseRequest, *fuse.DestroyRequest, *fuse.FlushRequest:
			resp = ""
			r.(interface{Respond()}).Respond()
		case *fuse.GetxattrRequest, *fuse.SetxattrRequest, *fuse.ListxattrRequest, *fuse.RemovexattrRequest:
			r.RespondError(syscall.ENOSYS)
			resp = syscall.ENOSYS
		}
		fmt.Printf("-> %#x %s\n", req.Hdr().ID, resp)
	}
}
