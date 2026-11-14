package oggvorbis

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"testing"
	"time"
)

// chunkReader returns at most limit bytes per Read call, so capture patterns
// and page headers are split across Read boundaries.
type chunkReader struct {
	data  []byte
	limit int
	pos   int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.limit {
		n = r.limit
	}
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

// seekChunkReader is a seekable chunkReader.
type seekChunkReader struct {
	*chunkReader
}

func (r *seekChunkReader) Seek(offset int64, whence int) (int64, error) {
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = int64(r.pos) + offset
	case io.SeekEnd:
		pos = int64(len(r.data)) + offset
	default:
		return 0, io.ErrUnexpectedEOF
	}
	if pos < 0 || pos > int64(len(r.data)) {
		return 0, io.ErrUnexpectedEOF
	}
	r.pos = int(pos)
	return pos, nil
}

// zeroReader always reports no progress and no error.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return 0, nil }

func capturePatternOffsets(data []byte) []int {
	var offsets []int
	for i := 0; i+len(capturePattern) <= len(data); i++ {
		if bytes.Equal(data[i:i+len(capturePattern)], capturePattern[:]) {
			offsets = append(offsets, i)
		}
	}
	return offsets
}

func readTestFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// within fails the test if f does not return within a few seconds, guarding
// against the busy loop this code used to contain.
func within(t *testing.T, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not terminate", what)
		return nil
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestRestoreFindPatterns checks that a page survives Restore intact no
// matter how the input is chunked, i.e. that a capture pattern split across
// two Read calls is joined and that the header, segment table and packet data
// are not lost.
func TestRestoreFindPatterns(t *testing.T) {
	data := readTestFile(t, "testdata/test.ogg")
	firstPage := data[:capturePatternOffsets(data)[1]]

	var want page
	if err := want.read(bytes.NewReader(firstPage)); err != nil {
		t.Fatal(err)
	}

	check := func(t *testing.T, src *chunkReader) {
		t.Helper()
		r := &oggReader{source: src}
		if err := within(t, "Restore", r.Restore); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if !bytes.HasPrefix(r.buffer, capturePattern[:]) {
			t.Fatalf("buffer does not start with capture pattern: %x", r.buffer[:minInt(16, len(r.buffer))])
		}
		var got page
		if err := got.read(r); err != nil {
			t.Fatalf("page read: %v", err)
		}
		if got.totalSize != want.totalSize || got.packetCount != want.packetCount ||
			got.AbsoluteGranulePosition != want.AbsoluteGranulePosition {
			t.Fatalf("page = {size %d, packets %d, granule %d}, want {size %d, packets %d, granule %d}",
				got.totalSize, got.packetCount, got.AbsoluteGranulePosition,
				want.totalSize, want.packetCount, want.AbsoluteGranulePosition)
		}
		for i := range want.packets {
			if !bytes.Equal(got.packets[i], want.packets[i]) {
				t.Fatalf("packet %d differs", i)
			}
		}
	}

	for _, limit := range []int{1, 2, 3, 4, 5, 7, 13, 1023, 1024, 1025, len(data)} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			check(t, &chunkReader{data: firstPage, limit: limit})
		})
	}

	// The pattern may start at any byte offset, with its tail in a later
	// Read call (limits 1..16 all split the 4-byte pattern somewhere).
	for offset := 0; offset < 8; offset++ {
		input := append(bytes.Repeat([]byte{0xc9}, offset), firstPage...)
		for _, limit := range []int{1, 2, 3, 4, 5, 7, 16} {
			t.Run("offset"+strconv.Itoa(offset)+"limit"+strconv.Itoa(limit), func(t *testing.T) {
				check(t, &chunkReader{data: input, limit: limit})
			})
		}
	}
}

// TestRestoreAcrossPages repeatedly runs Restore and reads the following page
// from a chunked stream, comparing every packet with a baseline parse. This
// covers capture patterns split at every Read boundary of a real stream.
func TestRestoreAcrossPages(t *testing.T) {
	data := readTestFile(t, "testdata/test.ogg")

	var baseline [][]byte
	base := &oggReader{source: bytes.NewReader(data)}
	for {
		var p page
		err := p.read(base)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		baseline = append(baseline, p.packets[:p.packetCount]...)
	}

	for _, limit := range []int{1, 2, 3, 4, 5, 7, 13, 64, 1024, len(data)} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			src := &chunkReader{data: data, limit: limit}
			r := &oggReader{source: src}
			var got [][]byte
			for {
				err := within(t, "Restore", r.Restore)
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					break
				}
				if err != nil {
					t.Fatalf("Restore: %v", err)
				}
				var p page
				if err := p.read(r); err != nil {
					t.Fatalf("page read: %v", err)
				}
				got = append(got, p.packets[:p.packetCount]...)
			}
			if len(got) != len(baseline) {
				t.Fatalf("decoded %d packets, want %d", len(got), len(baseline))
			}
			for i := range baseline {
				if !bytes.Equal(got[i], baseline[i]) {
					t.Fatalf("packet %d differs (%d != %d bytes)", i, len(got[i]), len(baseline[i]))
				}
			}
		})
	}
}

// TestRestoreTruncatedInput checks that Restore terminates with a stable
// error when the input ends before a complete capture pattern, keeps at most
// three bytes and behaves the same when called again after a failure.
func TestRestoreTruncatedInput(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, io.EOF},
		{"one byte", []byte{'O'}, io.ErrUnexpectedEOF},
		{"two bytes", []byte{'O', 'g'}, io.ErrUnexpectedEOF},
		{"three bytes", []byte{'O', 'g', 'g'}, io.ErrUnexpectedEOF},
		{"damaged", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xc9, 0x03}, io.ErrUnexpectedEOF},
		{"junk then partial", []byte("junkOgg"), io.ErrUnexpectedEOF},
		{"garbage", bytes.Repeat([]byte{0x00, 0xff, 0xc9, 0x03}, 1000), io.ErrUnexpectedEOF},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, limit := range []int{1, 2, 3, 4, 7, 1024} {
				r := &oggReader{source: &chunkReader{data: c.data, limit: limit}}
				for call := 1; call <= 2; call++ {
					err := within(t, "Restore", r.Restore)
					if err != c.want {
						t.Fatalf("limit %d call %d: Restore = %v, want %v", limit, call, err, c.want)
					}
					if len(r.buffer) > len(capturePattern)-1 {
						t.Fatalf("limit %d call %d: retained %d bytes, want at most %d",
							limit, call, len(r.buffer), len(capturePattern)-1)
					}
				}
			}
		})
	}
}

// TestRestoreNoProgress makes sure a reader that reports neither data nor an
// error cannot turn Restore into a busy loop.
func TestRestoreNoProgress(t *testing.T) {
	r := &oggReader{source: zeroReader{}}
	if err := within(t, "Restore", r.Restore); err != io.ErrNoProgress {
		t.Fatalf("Restore = %v, want io.ErrNoProgress", err)
	}
}

// TestLastPositionChunked exercises Restore in its real call path: seeking to
// the end of a stream that returns short reads must still find the length.
func TestLastPositionChunked(t *testing.T) {
	data := readTestFile(t, "testdata/test.ogg")
	for _, limit := range []int{1, 2, 3, 4, 5, 7, 13, 1024, len(data)} {
		r := &oggReader{source: &seekChunkReader{&chunkReader{data: data, limit: limit}}}
		r.seeker = r.source.(io.Seeker)
		var length int64
		err := within(t, "LastPosition", func() error {
			var err error
			length, err = r.LastPosition()
			return err
		})
		if err != nil {
			t.Fatalf("limit %d: LastPosition: %v", limit, err)
		}
		if length != 44100 {
			t.Fatalf("limit %d: length = %d, want 44100", limit, length)
		}
	}
}
