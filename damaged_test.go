package oggvorbis_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/jfreymuth/oggvorbis"
)

// noSeek hides the io.Seeker implementation of the wrapped reader.
type noSeek struct{ io.Reader }

// TestDamagedInputTerminates makes sure malformed input without a capture
// pattern is rejected in bounded time instead of spinning in the recovery
// code, both for seekable input (which uses Restore for the length) and for
// plain readers.
func TestDamagedInputTerminates(t *testing.T) {
	cases := map[string][]byte{
		"given":       {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xc9, 0x03},
		"empty":       {},
		"one byte":    {'O'},
		"two bytes":   {'O', 'g'},
		"three bytes": {'O', 'g', 'g'},
		"garbage":     bytes.Repeat([]byte{0x00, 0xff, 0xc9, 0x03}, 2000),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			for _, seekable := range []bool{true, false} {
				in := io.Reader(bytes.NewReader(data))
				if !seekable {
					in = noSeek{bytes.NewReader(data)}
				}
				done := make(chan error, 1)
				go func() {
					_, err := oggvorbis.NewReader(in)
					done <- err
				}()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("NewReader accepted damaged input")
					}
					if err != io.EOF && err != io.ErrUnexpectedEOF && err.Error() != "ogg: missing capture pattern" {
						t.Fatalf("unexpected error: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("NewReader did not terminate (seekable=%v)", seekable)
				}
			}
		})
	}
}

// TestGetLengthDamagedInput covers GetLength, which restores from the end of
// a seekable stream before it can determine the length.
func TestGetLengthDamagedInput(t *testing.T) {
	data := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xc9, 0x03}
	done := make(chan error, 1)
	go func() {
		_, _, err := oggvorbis.GetLength(bytes.NewReader(data))
		done <- err
	}()
	select {
	case err := <-done:
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			t.Fatalf("GetLength error = %v, want EOF or ErrUnexpectedEOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetLength did not terminate")
	}
}
