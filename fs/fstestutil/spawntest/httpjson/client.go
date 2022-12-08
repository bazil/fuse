package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// JSON helps make a HTTP request to the resource tree at url with
// JSON request and response bodies.
//
// If client is nil http.DefaultClient will be used.
func JSON(client *http.Client, url string) *Resource {
	if client == nil {
		client = http.DefaultClient
	}
	return &Resource{
		http: client,
		url:  url,
	}
}

// Resource represents a JSON-speaking remote HTTP resource.
type Resource struct {
	http *http.Client
	url  string
}

// Call a HTTP resource that is expected to return JSON data.
//
// If data is not nil, method is POST and the request body is data
// marshaled into JSON. If data is nil, method is GET.
//
// The response JSON is unmarshaled into dst.
func (c *Resource) Call(ctx context.Context, data interface{}, dst interface{}) error {
	method := "GET"
	var body io.Reader
	if data != nil {
		buf, err := json.Marshal(data)
		if err != nil {
			return err
		}
		method = "POST"
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url, body)
	if err != nil {
		return err
	}
	if method != "GET" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf, err := io.ReadAll(resp.Body)
		if err != nil {
			buf = []byte("(cannot read error body: " + err.Error() + ")")
		}
		return fmt.Errorf("http error: %v: %q", resp.Status, bytes.TrimSpace(buf))
	}

	dec := json.NewDecoder(resp.Body)
	// add options to func JSON to disable this, if need is strong
	// enough; at that point might change api to just take nothing but
	// options, use default for client etc.
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := mustEOF(dec); err != nil {
		return err
	}
	return nil
}
