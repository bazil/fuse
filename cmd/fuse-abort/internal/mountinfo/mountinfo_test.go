package mountinfo_test

import (
	"io"
	"os"
	"testing"

	"bazil.org/fuse/cmd/fuse-abort/internal/mountinfo"
)

func TestOpenError(t *testing.T) {
	r, err := mountinfo.Open("testdata/does-not-exist")
	if err == nil {
		r.Close()
		t.Fatal("expected an error")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected a not-exists error: %v", err)
	}
}

func TestReal(t *testing.T) {
	r, err := mountinfo.Open("testdata/fuzz/corpus/real.mountinfo")
	if err != nil {
		t.Fatalf("cannot open mountinfo: %v", err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	success := false
	for {
		info, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading mountinfo: %v", err)
		}
		if info.Mountpoint != "/home/tv/tmp/fusetest128387706" {
			t.Logf("skip %#v\n", info)
			continue
		}
		t.Logf("found %#v\n", info)
		success = true
		if g, e := info.Major, "0"; g != e {
			t.Errorf("wrong major number: %q != %q", g, e)
		}
		if g, e := info.Minor, "139"; g != e {
			t.Errorf("wrong minor number: %q != %q", g, e)
		}
		if g, e := info.FSType, "fuse"; g != e {
			t.Errorf("wrong FS type: %q != %q", g, e)
		}
	}
	if !success {
		t.Error("did not see mount")
	}
}

func TestEscape(t *testing.T) {
	r, err := mountinfo.Open("testdata/fuzz/corpus/escaped.mountinfo")
	if err != nil {
		t.Fatalf("cannot open mountinfo: %v", err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	info, err := r.Next()
	if err != nil {
		t.Fatalf("reading mountinfo: %v", err)
	}
	t.Logf("got %#v\n", info)
	if g, e := info.Mountpoint, "/xyz\x53zy"; g != e {
		t.Errorf("wrong mountpoint: %q != %q", g, e)
	}
	if g, e := info.FSType, "foo\x01bar"; g != e {
		t.Errorf("wrong FS type: %q != %q", g, e)
	}
}

func TestCrashers(t *testing.T) {
	r, err := mountinfo.Open("testdata/fuzz/corpus/crashers.mountinfo")
	if err != nil {
		t.Fatalf("cannot open mountinfo: %v", err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	for {
		_, err := r.Next()
		if err == io.EOF {
			break
		}
		// we don't care about errors, just that we don't crash
	}
}
