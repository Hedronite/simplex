// Package failure carries the exit code a failure maps to. Everything below
// main returns these (or plain errors, which count as runtime failures); only
// main turns them into an exit code and an error line.
package failure

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Hedronite/simplex/internal/exitcode"
)

// Error is a failure with the exit code it maps to.
type Error struct {
	Code    int
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Config is a configuration or usage error (exit 3).
func Config(format string, args ...any) *Error {
	return &Error{Code: exitcode.Config, Message: fmt.Sprintf(format, args...)}
}

// Runtime is a runtime or I/O error (exit 4).
func Runtime(err error, format string, args ...any) *Error {
	return &Error{Code: exitcode.Runtime, Message: fmt.Sprintf(format, args...), Err: err}
}

// CodeOf returns the exit code for any error. An error that is not an *Error
// is a runtime failure. It never returns 0, 1 or 2.
func CodeOf(err error) int {
	var f *Error
	if errors.As(err, &f) && (f.Code == exitcode.Config || f.Code == exitcode.Runtime) {
		return f.Code
	}
	return exitcode.Runtime
}

type errorLine struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Report writes the failure to w, one line. With asJSON it is the object
// {"error":{"code","message"}}; otherwise plain text.
func Report(w io.Writer, err error, asJSON bool) {
	code := CodeOf(err)
	if !asJSON {
		_, _ = fmt.Fprintf(w, "simplex: %s\n", err.Error())
		return
	}
	b, merr := json.Marshal(errorLine{Error: errorBody{Code: code, Message: err.Error()}})
	if merr != nil {
		// The struct has only an int and a string, so this cannot fail.
		panic(merr)
	}
	_, _ = fmt.Fprintf(w, "%s\n", b)
}
