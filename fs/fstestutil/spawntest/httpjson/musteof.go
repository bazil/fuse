package httpjson

import (
	"encoding/json"
	"fmt"
	"io"
)

// TrailingDataError is an error that is returned if there is trailing
// data after a JSON message.
type TrailingDataError struct {
	Token json.Token
}

func (t *TrailingDataError) Error() string {
	return fmt.Sprintf("invalid character %q after top-level value", t.Token)
}

func mustEOF(dec *json.Decoder) error {
	token, err := dec.Token()
	switch err {
	case io.EOF:
		// expected
		return nil
	case nil:
		return &TrailingDataError{Token: token}
	default:
		return err
	}
}
