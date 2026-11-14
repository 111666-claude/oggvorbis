package oggvorbis_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"testing"

	"github.com/jfreymuth/oggvorbis"
)

type reader struct {
	io.Reader
}

func (r reader) Read(p []byte) (int, error) {
	return r.Reader.Read(p)
}

func test(t *testing.T, filename string, reference []float32, tolerance float32, wrap bool) {
	ogg, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer ogg.Close()
	var r io.Reader
	if wrap {
		// wrap the file so it doesn't implement io.Seeker
		r = reader{ogg}
	} else {
		r = ogg
	}
	dec, _, err := oggvorbis.ReadAll(r)
	if err != nil {
		t.Fatal("decoding error: ", err)
	}

	compare(t, dec, reference, tolerance)
}

func TestRead(t *testing.T) {
	raw, err := os.Open("testdata/test.raw")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	rawSize, _ := raw.Seek(0, io.SeekEnd)
	raw.Seek(0, io.SeekStart)
	rawData := make([]float32, rawSize/4)
	binary.Read(raw, binary.LittleEndian, rawData)

	test(t, "testdata/test.ogg", rawData, 0.00002, false)
	test(t, "testdata/test.ogg", rawData, 0.00002, true)
}

func TestFormat(t *testing.T) {
	ogg, err := os.Open("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	defer ogg.Close()

	format, err := oggvorbis.GetFormat(ogg)
	if err != nil {
		t.Fatal(err)
	}
	if format.SampleRate != 44100 {
		t.Errorf("sample rate is %d, expected %d", format.SampleRate, 44100)
	}
	if format.Channels != 1 {
		t.Errorf("channels is %d, expected %d", format.Channels, 1)
	}
}

func TestLength(t *testing.T) {
	ogg, err := os.Open("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	defer ogg.Close()

	length, _, err := oggvorbis.GetLength(ogg)
	if err != nil {
		t.Fatal(err)
	}
	if length != 44100 {
		t.Errorf("length is %d, expected %d", length, 44100)
	}
}

func TestLengthForUnexpectedEof(t *testing.T) {
	ogg, err := os.Open("testdata/eof_issue.ogg")
	if err != nil {
		t.Fatal(err)
	}
	defer ogg.Close()

	length, _, err := oggvorbis.GetLength(ogg)
	if err != nil {
		t.Fatal(err)
	}

	const expectedLength = 72384
	if length != expectedLength {
		t.Errorf("length is %d, expected %d", length, expectedLength)
	}
	r, err := oggvorbis.NewReader(ogg)
	if err != nil {
		t.Fatal(err)
	}

	if r.Length() != expectedLength {
		t.Fatalf("length is %d, expected %d", r.Length(), expectedLength)
	}
}

func TestSeek(t *testing.T) {
	ogg, err := os.Open("testdata/long.ogg")
	if err != nil {
		t.Fatal(err)
	}
	defer ogg.Close()

	data, _, err := oggvorbis.ReadAll(ogg)
	if err != nil {
		t.Fatal(err)
	}
	ogg.Seek(0, io.SeekStart)

	r, err := oggvorbis.NewReader(ogg)
	if err != nil {
		t.Fatal(err)
	}

	if int(r.Length()) != len(data) {
		t.Fatalf("length is %d, expected %d", r.Length(), len(data))
	}

	sections := []struct{ start, len int64 }{
		{600000, 10000},
		// test first position
		{0, 500},
		// test last position
		{881999, 1},
		// test around page boundary
		{182399, 100},
		{182400, 100},
		{182401, 100},
		// seek to an out of bounds position and then continue
		{1000000, 0},
		{200000, 1000},
	}
test:
	for _, s := range sections {
		err := r.SetPosition(s.start)
		if err != nil {
			t.Error(err)
			continue test
		}
		if s.len == 0 {
			continue test
		}
		ref := data[s.start : s.start+s.len]
		buf := make([]float32, s.len)
		for n := 0; n < len(buf); {
			read, err := r.Read(buf[n:])
			if err != nil {
				t.Error(err)
				continue test
			}
			n += read
		}
		compare(t, buf, ref, .00001)
	}

	r.SetPosition(882000)
	_, err = r.Read(nil)
	if err != io.EOF {
		t.Errorf("error should be EOF, but is %s", err)
	}
}

func TestEmpty(t *testing.T) {
	_, err := oggvorbis.NewReader(bytes.NewReader(nil))
	if err != io.ErrUnexpectedEOF {
		t.Errorf("error should be unexpected EOF, but is %s", err)
	}
}

// TestSeekableVsNonSeekable decodes the same file through a seekable and
// a non-seekable source and requires identical headers and sample output
// (the seekable output may be shorter when the stream lacks an
// EndOfStream flag, since the probed length truncates the tail).
func TestSeekableVsNonSeekable(t *testing.T) {
	for _, filename := range []string{"testdata/test.ogg", "testdata/long.ogg", "testdata/eof_issue.ogg"} {
		t.Run(filename, func(t *testing.T) {
			ogg, err := os.Open(filename)
			if err != nil {
				t.Fatal(err)
			}
			defer ogg.Close()
			sr, err := oggvorbis.NewReader(ogg)
			if err != nil {
				t.Fatal("seekable NewReader: ", err)
			}

			ogg2, err := os.Open(filename)
			if err != nil {
				t.Fatal(err)
			}
			defer ogg2.Close()
			nr, err := oggvorbis.NewReader(reader{ogg2})
			if err != nil {
				t.Fatal("non-seekable NewReader: ", err)
			}

			if sr.SampleRate() != nr.SampleRate() || sr.Channels() != nr.Channels() {
				t.Errorf("format mismatch: seekable %d/%d, non-seekable %d/%d",
					sr.SampleRate(), sr.Channels(), nr.SampleRate(), nr.Channels())
			}
			sc, nc := sr.CommentHeader(), nr.CommentHeader()
			if sc.Vendor != nc.Vendor || len(sc.Comments) != len(nc.Comments) {
				t.Errorf("comment header mismatch: %q %v vs %q %v",
					sc.Vendor, sc.Comments, nc.Vendor, nc.Comments)
			}

			sSamples := readAll(t, sr)
			nSamples := readAll(t, nr)
			if len(sSamples) > len(nSamples) {
				t.Fatalf("seekable decoded %d samples, non-seekable only %d",
					len(sSamples), len(nSamples))
			}
			compareExact(t, sSamples, nSamples[:len(sSamples)])
		})
	}
}

// TestRepeatedReaders checks that decoding the same data twice gives
// identical results and that reads after EOF keep returning EOF instead
// of repeating packets.
func TestRepeatedReaders(t *testing.T) {
	data, err := os.ReadFile("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}

	decode := func(t *testing.T) *oggvorbis.Reader {
		r, err := oggvorbis.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	r1 := decode(t)
	first := readAll(t, r1)
	r2 := decode(t)
	second := readAll(t, r2)
	compareExact(t, first, second)

	if want := 44100 * r1.Channels(); len(first) != want {
		t.Fatalf("decoded %d values, expected %d", len(first), want)
	}

	// reads after EOF must not produce more data
	buf := make([]float32, 512)
	for i := 0; i < 3; i++ {
		n, err := r1.Read(buf)
		if n != 0 || err != io.EOF {
			t.Errorf("read %d after EOF: n=%d, err=%v, expected 0, EOF", i, n, err)
		}
	}
}

func compareExact(t *testing.T, a, e []float32) {
	t.Helper()
	if len(a) != len(e) {
		t.Fatalf("length of output is %d, expected %d", len(a), len(e))
	}
	for i := range a {
		if a[i] != e[i] {
			t.Fatalf("different values at index %d (%g != %g)", i, a[i], e[i])
		}
	}
}

func readAll(t *testing.T, r *oggvorbis.Reader) []float32 {
	t.Helper()
	buf := make([]float32, 4096)
	var out []float32
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal("read error: ", err)
		}
		if n == 0 {
			t.Fatal("no progress")
		}
	}
}

func compare(t *testing.T, a, e []float32, tolerance float32) {
	if len(a) != len(e) {
		t.Fatalf("length of output is %d, expected %d", len(a), len(e))
	}
	for i := range a {
		if !equal(a[i], e[i], tolerance) {
			t.Errorf("different values at index %d (%g != %g)", i, a[i], e[i])
			break
		}
	}
}

func equal(a, b, tolerance float32) bool {
	return (a > b && a-b < tolerance) || b-a < tolerance
}
