package claudemaintenance

import (
	"io"
	"strings"
	"testing"
)

// exec.Cmd copies pipe output from a reader without WriterTo. Exercise that
// exact io.Copy dispatch, rather than calling our Write method directly.
type outputReaderOnly struct{ io.Reader }

func TestOutputCopyObservesHandshakeAndResult(t *testing.T) {
	ready, done := 0, 0
	w := &resultOutput{
		limitedOutput: limitedOutput{limit: 4096},
		ready:         func() { ready++ },
		done:          func() { done++ },
	}
	stream := "{\"type\":\"system\",\"subtype\":\"init\"}\n" +
		"{\"type\":\"result\",\"is_error\":false}\n"
	for range 2 {
		if _, err := io.Copy(w, outputReaderOnly{strings.NewReader(stream)}); err != nil {
			t.Fatal(err)
		}
	}
	if ready != 1 || done != 1 || string(w.Bytes()) != stream+stream {
		t.Fatalf("ready=%d done=%d bytes=%d", ready, done, w.Len())
	}
}

func TestOutputCopyCannotBypassLimits(t *testing.T) {
	for _, result := range []bool{false, true} {
		var w interface {
			io.Writer
			Len() int
		}
		if result {
			w = &resultOutput{limitedOutput: limitedOutput{limit: 8}, ready: func() {}, done: func() {}}
		} else {
			w = &limitedOutput{limit: 8}
		}
		if _, err := io.Copy(w, outputReaderOnly{strings.NewReader("0123456789")}); err == nil {
			t.Fatalf("result=%v: output limit was bypassed", result)
		}
		if w.Len() > 8 {
			t.Fatalf("result=%v: buffered %d bytes", result, w.Len())
		}
	}
}
