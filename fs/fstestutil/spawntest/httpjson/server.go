package httpjson

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
)

// TODO ServeGET

// ServePOST adapts a function to a http.Handler with easy JSON
// unmarshal & marshal and error reporting.
//
// fn is expected to be of the form
//
//	func(context.Context, T1) (T2, error)
//
// or similar with pointers to T1 or T2.
func ServePOST(fn interface{}) http.Handler {
	val := reflect.ValueOf(fn)
	if val.Kind() != reflect.Func {
		panic("JSONHandler was passed a value that is not a function")
	}
	typ := val.Type()
	if typ.NumIn() != 2 {
		panic("JSONHandler function must take two arguments")
	}
	if typ.In(0) != reflect.TypeOf((*context.Context)(nil)).Elem() {
		panic("JSONHandler function must take context")
	}
	if typ.NumOut() != 2 {
		panic("JSONHandler function must return two values")
	}
	if typ.Out(1) != reflect.TypeOf((*error)(nil)).Elem() {
		panic("JSONHandler function must return error")
	}

	return jsonPOST{
		fnVal:   val,
		argType: typ.In(1),
		retType: typ.Out(0),
	}
}

type jsonPOST struct {
	fnVal   reflect.Value
	argType reflect.Type
	retType reflect.Type
}

var _ http.Handler = jsonPOST{}

func (j jsonPOST) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		w.Header().Set("Allow", "POST")
		const code = http.StatusMethodNotAllowed
		http.Error(w, http.StatusText(code), code)
		return
	}

	// TODO do we want to enforce request Content-Type

	// TODO do we want to enforce request Accept

	argVal := reflect.New(j.argType)
	arg := argVal.Interface()
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(arg); err != nil {
		msg := fmt.Sprintf("cannot unmarshal request body as json: %v", err)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if err := mustEOF(dec); err != nil {
		msg := fmt.Sprintf("cannot unmarshal request body as json: %v", err)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	ret := j.fnVal.Call([]reflect.Value{
		reflect.ValueOf(req.Context()),
		argVal.Elem(),
	})

	// TODO allow bad request etc status codes
	errI := ret[1].Interface()
	if errI != nil {
		if err := errI.(error); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	data := ret[0].Interface()
	buf, err := json.Marshal(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(buf); err != nil {
		panic(http.ErrAbortHandler)
	}
}
