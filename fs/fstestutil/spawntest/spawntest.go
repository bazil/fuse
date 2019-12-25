// Package spawntest helps write tests that use subprocesses.
//
// The subprocess runs a HTTP server on a UNIX domain socket, and the
// test can make HTTP requests to control the behavior of the helper
// subprocess.
//
// Helpers are identified by names they pass to Registry.Register.
// This call should be placed in an init function. The test spawns the
// subprocess by executing the same test binary in a subprocess,
// passing it a special flag that is recognized by TestMain.
//
// This might get extracted to a standalone repository, if it proves
// useful enough.
package spawntest

import (
	"context"
	"errors"
	"flag"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"bazil.org/fuse/fs/fstestutil/spawntest/httpjson"
	"github.com/tv42/httpunix"
)

// Registry keeps track of helpers.
//
// The zero value is ready to use.
type Registry struct {
	mu         sync.Mutex
	helpers    map[string]http.Handler
	runName    string
	runHandler http.Handler
}

// Register a helper in the registry.
//
// This should be called from a top-level variable assignment.
//
// Register will panic if the name is already registered.
func (r *Registry) Register(name string, h http.Handler) *Helper {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.helpers == nil {
		r.helpers = make(map[string]http.Handler)
	}
	if _, seen := r.helpers[name]; seen {
		panic("spawntest: helper already registered: " + name)
	}
	r.helpers[name] = h
	hh := &Helper{
		name: name,
	}
	return hh
}

type helperFlag struct {
	r *Registry
}

var _ flag.Value = helperFlag{}

func (hf helperFlag) String() string {
	if hf.r == nil {
		return ""
	}
	return hf.r.runName
}

func (hf helperFlag) Set(s string) error {
	h, ok := hf.r.helpers[s]
	if !ok {
		return errors.New("helper not found")
	}
	hf.r.runName = s
	hf.r.runHandler = h
	return nil
}

const flagName = "spawntest.internal.helper"

// AddFlag adds the command-line flag used to communicate between
// Control and the helper to the flag set. Typically flag.CommandLine
// is used, and this should be called from TestMain before flag.Parse.
func (r *Registry) AddFlag(f *flag.FlagSet) {
	v := helperFlag{r: r}
	f.Var(v, flagName, "internal use only")
}

// RunIfNeeded passes execution to the helper if the right
// command-line flag was seen. This should be called from TestMain
// after flag.Parse. If running as the helper, the call will not
// return.
func (r *Registry) RunIfNeeded() {
	h := r.runHandler
	if h == nil {
		return
	}
	f := os.NewFile(3, "<control-socket>")
	l, err := net.FileListener(f)
	if err != nil {
		log.Fatalf("cannot listen: %v", err)
	}
	if err := http.Serve(l, h); err != nil {
		log.Fatalf("http server error: %v", err)
	}
	os.Exit(0)
}

// Helper is the result of registering a helper. It can be used by
// tests to spawn the helper.
type Helper struct {
	name string
}

type transportWithBase struct {
	Base      *url.URL
	Transport http.RoundTripper
}

var _ http.RoundTripper = (*transportWithBase)(nil)

func (t *transportWithBase) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	req = req.Clone(ctx)
	req.URL = t.Base.ResolveReference(req.URL)
	return t.Transport.RoundTrip(req)
}

func makeHTTPClient(path string) *http.Client {
	u := &httpunix.Transport{}
	const loc = "helper"
	u.RegisterLocation(loc, path)
	t := &transportWithBase{
		Base: &url.URL{
			Scheme: httpunix.Scheme,
			Host:   loc,
		},
		Transport: u,
	}
	client := &http.Client{
		Transport: t,
	}
	return client
}

// Spawn the helper. All errors will be reported via t.Logf and fatal
// errors result in t.FailNow. The helper is killed after context
// cancels.
func (h *Helper) Spawn(ctx context.Context, t testing.TB) *Control {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("spawntest: cannot find our executable: %v", err)
	}

	// could use TB.TempDir()
	// https://github.com/golang/go/issues/35998
	dir, err := ioutil.TempDir("", "spawntest")
	if err != nil {
		t.Fatalf("spawnmount.Spawn: cannot make temp dir: %v", err)
	}
	defer func() {
		if dir != "" {
			if err := os.RemoveAll(dir); err != nil {
				t.Logf("error cleaning temp dir: %v", err)
			}
		}
	}()

	controlPath := filepath.Join(dir, "control")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: controlPath, Net: "unix"})
	if err != nil {
		t.Fatalf("cannot open listener: %v", err)
	}
	l.SetUnlinkOnClose(false)
	defer l.Close()

	lf, err := l.File()
	if err != nil {
		t.Fatalf("cannot get FD from listener: %v", err)
	}
	defer lf.Close()

	cmd := exec.Command(executable, "-"+flagName+"="+h.name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{lf}

	if err := cmd.Start(); err != nil {
		t.Fatalf("spawntest: cannot start helper: %v", err)
	}
	defer func() {
		if cmd != nil {
			if err := cmd.Process.Kill(); err != nil {
				t.Logf("error killing spawned helper: %v", err)
			}
		}
	}()

	c := &Control{
		t:    t,
		dir:  dir,
		cmd:  cmd,
		http: makeHTTPClient(controlPath),
	}
	dir = ""
	cmd = nil
	return c
}

// Control an instance of a helper running as a subprocess.
type Control struct {
	t    testing.TB
	dir  string
	cmd  *exec.Cmd
	http *http.Client
}

// Close kills the helper and frees resources.
func (c *Control) Close() {
	if c.cmd.ProcessState == nil {
		// not yet Waited on
		c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}

	if c.dir != "" {
		if err := os.RemoveAll(c.dir); err != nil {
			c.t.Logf("error cleaning temp dir: %v", err)
		}
		c.dir = ""
	}
}

// Signal send a signal to the helper process.
func (c *Control) Signal(sig os.Signal) error {
	return c.cmd.Process.Signal(sig)
}

// HTTP returns a HTTP client that can be used to communicate with the
// helper. URLs passed to this helper should not include scheme or
// host.
func (c *Control) HTTP() *http.Client {
	return c.http
}

// JSON returns a helper to make HTTP requests that pass data as JSON
// to the resource identified by path. Path should not include scheme
// or host. Path can be empty to communicate with the root resource.
func (c *Control) JSON(path string) *httpjson.Resource {
	return httpjson.JSON(c.http, path)
}
