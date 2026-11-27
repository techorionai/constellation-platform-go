package audit

import (
	"bytes"
	"errors"
	"testing"
)

// U6: the size limit. The other rules keep a CloudEvent far below 64 KiB, so
// the test checks the size rule directly.
func TestU6EntrySize(t *testing.T) {
	exact := bytes.Repeat([]byte("a"), maxEntryBytes)
	if err := checkPayload(exact); err != nil {
		t.Errorf("64 KiB: %v", err)
	}
	if err := checkPayload(append(exact, 'a')); !errors.Is(err, ErrEntryTooLarge) {
		t.Errorf("64 KiB + 1: err = %v, want ErrEntryTooLarge", err)
	}
}

func TestEscapedNUL(t *testing.T) {
	cases := map[string]bool{
		`{"a":"\u0000"}`:   true,
		`{"a":"\\u0000"}`:  false,
		`{"a":"\\\u0000"}`: true,
		`{"a":"x"}`:        false,
	}
	for in, want := range cases {
		if got := hasEscapedNUL([]byte(in)); got != want {
			t.Errorf("hasEscapedNUL(%s) = %v, want %v", in, got, want)
		}
	}
}
