package oggvorbis_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/jfreymuth/oggvorbis"
)

// TestSeekIndexSampleAccuracy seeks to many positions of the fixed-length
// long.ogg fixture (882000 samples) with and without the seek index and
// compares the decoded PCM against a sequential decode of the same file.
func TestSeekIndexSampleAccuracy(t *testing.T) {
	ogg, err := os.Open("testdata/long.ogg")
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := oggvorbis.ReadAll(ogg)
	ogg.Close()
	if err != nil {
		t.Fatal(err)
	}
	const total = 882000
	if len(data) != total {
		t.Fatalf("reference has %d samples, expected %d", len(data), total)
	}

	for _, interval := range []int64{-1, 4096, oggvorbis.DefaultSeekIndexInterval} {
		ogg, err := os.Open("testdata/long.ogg")
		if err != nil {
			t.Fatal(err)
		}
		r, err := oggvorbis.NewReaderWithSeekIndex(ogg, interval)
		if err != nil {
			ogg.Close()
			t.Fatal(err)
		}
		if r.Length() != total {
			t.Fatalf("length is %d, expected %d", r.Length(), total)
		}
		for pos := int64(0); pos < total; pos += 9973 {
			if err := r.SetPosition(pos); err != nil {
				t.Fatalf("interval %d: SetPosition(%d): %s", interval, pos, err)
			}
			if p := r.Position(); p != pos {
				t.Fatalf("interval %d: position after SetPosition(%d) is %d", interval, pos, p)
			}
			n := int64(300)
			if pos+n > total {
				n = total - pos
			}
			buf := make([]float32, n)
			for got := 0; got < len(buf); {
				m, err := r.Read(buf[got:])
				got += m
				if err != nil {
					t.Fatalf("interval %d: read at %d: %s", interval, pos, err)
				}
			}
			compare(t, buf, data[pos:pos+n], .00001)
		}
		ogg.Close()
	}
}

// TestSeekIndexOutOfRange checks that out-of-range positions have defined
// behavior: negative positions are an error, positions at or past the end
// report EOF on the next read.
func TestSeekIndexOutOfRange(t *testing.T) {
	ogg, err := os.Open("testdata/long.ogg")
	if err != nil {
		t.Fatal(err)
	}
	defer ogg.Close()
	r, err := oggvorbis.NewReader(ogg)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetPosition(-1); err == nil {
		t.Fatal("negative position should return an error")
	}
	if err := r.SetPosition(r.Length() + 1000); err != nil {
		t.Fatalf("seek past the end should not fail: %s", err)
	}
	if _, err := r.Read(make([]float32, 100)); err != io.EOF {
		t.Fatalf("error should be EOF, but is %v", err)
	}
	// The reader stays usable after an out-of-range seek.
	if err := r.SetPosition(100); err != nil {
		t.Fatal(err)
	}
	if p := r.Position(); p != 100 {
		t.Fatalf("position is %d, expected 100", p)
	}
}

// TestSeekIndexNotSeekable checks that seeking a non-seekable input fails
// cleanly (and no index is built for it).
func TestSeekIndexNotSeekable(t *testing.T) {
	ogg, err := os.Open("testdata/long.ogg")
	if err != nil {
		t.Fatal(err)
	}
	defer ogg.Close()
	r, err := oggvorbis.NewReaderWithSeekIndex(reader{ogg}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetPosition(100); err == nil {
		t.Fatal("SetPosition on a non-seekable reader should return an error")
	}
}

// TestSeekIndexDefaultConstructor makes sure NewReader (which builds an
// index with the default interval) seeks correctly on the fixture.
func TestSeekIndexDefaultConstructor(t *testing.T) {
	data, err := os.ReadFile("testdata/long.ogg")
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := oggvorbis.ReadAll(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r, err := oggvorbis.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, pos := range []int64{0, 1, 182399, 182400, 182401, 600000, 881999} {
		if err := r.SetPosition(pos); err != nil {
			t.Fatalf("SetPosition(%d): %s", pos, err)
		}
		n := int64(200)
		if pos+n > int64(len(ref)) {
			n = int64(len(ref)) - pos
		}
		buf := make([]float32, n)
		for got := 0; got < len(buf); {
			m, err := r.Read(buf[got:])
			got += m
			if err != nil {
				t.Fatalf("read at %d: %s", pos, err)
			}
		}
		compare(t, buf, ref[pos:pos+n], .00001)
	}
}
