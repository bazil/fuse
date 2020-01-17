package spawntest_test

import (
	"context"
	"errors"
	"flag"
	"os"
	"testing"

	"bazil.org/fuse/fs/fstestutil/spawntest"
	"bazil.org/fuse/fs/fstestutil/spawntest/httpjson"
)

var helpers spawntest.Registry

type addRequest struct {
	A uint64
	B uint64
}

type addResult struct {
	X uint64
}

func add(ctx context.Context, req addRequest) (*addResult, error) {
	// In real tests, you'd instruct the helper to interact with the
	// system-under-test on behalf of the unit test process. For
	// brevity, we'll just do the action directly in this example.
	x := req.A + req.B
	if x < req.A {
		return nil, errors.New("overflow")
	}
	r := &addResult{
		X: x,
	}
	return r, nil
}

// The second argument to Register can be any http.Handler. To keep
// state in the helper between calls, you can create a custom type and
// delegate to methods based on http.Request.URL.Path.
var addHelper = helpers.Register("add", httpjson.ServePOST(add))

func name_me_TestAdd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	control := addHelper.Spawn(ctx, t)
	defer control.Close()

	var got addResult
	if err := control.JSON("/").Call(ctx, addRequest{A: 42, B: 13}, &got); err != nil {
		t.Fatalf("calling helper: %v", err)
	}
	if g, e := got.X, uint64(55); g != e {
		t.Errorf("wrong add result: %v != %v", g, e)
	}
}

func name_me_TestMain(m *testing.M) {
	helpers.AddFlag(flag.CommandLine)
	flag.Parse()
	helpers.RunIfNeeded()
	os.Exit(m.Run())
}

func Example() {}

// Quiet linters. See https://github.com/dominikh/go-tools/issues/675
var _ = name_me_TestAdd
var _ = name_me_TestMain
