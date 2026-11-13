package failure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Hedronite/simplex/internal/exitcode"
)

func TestCodeOfNeverReturnsASeverityCode(t *testing.T) {
	cases := map[string]error{
		"config":       Config("bad flag"),
		"runtime":      Runtime(errors.New("disk"), "write"),
		"plain":        errors.New("plain"),
		"wrapped":      fmt.Errorf("outer: %w", Config("inner")),
		"nil-wrapped":  &Error{Code: exitcode.Green, Message: "bogus"},
		"red-as-error": &Error{Code: exitcode.Red, Message: "bogus"},
	}
	want := map[string]int{
		"config": exitcode.Config, "runtime": exitcode.Runtime, "plain": exitcode.Runtime,
		"wrapped": exitcode.Config, "nil-wrapped": exitcode.Runtime, "red-as-error": exitcode.Runtime,
	}
	for name, err := range cases {
		got := CodeOf(err)
		if got != want[name] {
			t.Errorf("%s: code %d, want %d", name, got, want[name])
		}
		if got < exitcode.Config {
			t.Errorf("%s: error path exited %d", name, got)
		}
	}
}

func TestReportJSONIsOneLine(t *testing.T) {
	var b bytes.Buffer
	Report(&b, Config("config hosts.yaml: unknown key %q", "pot"), true)
	out := b.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("not exactly one line: %q", out)
	}
	var got struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Code != exitcode.Config || !strings.Contains(got.Error.Message, "unknown key") {
		t.Fatalf("unexpected %+v", got)
	}
}

func TestReportPlain(t *testing.T) {
	var b bytes.Buffer
	Report(&b, Runtime(errors.New("no space"), "write out file"), false)
	if got := b.String(); got != "simplex: write out file: no space\n" {
		t.Fatalf("%q", got)
	}
}
