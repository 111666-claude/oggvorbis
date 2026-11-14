package oggvorbis_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
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
	for i := 0; i < 3; i++ {
		r, err := oggvorbis.NewReader(bytes.NewReader(nil))
		if err != io.ErrUnexpectedEOF {
			t.Fatalf("attempt %d: error should be unexpected EOF, but is %v", i, err)
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("attempt %d: errors.Is(err, io.ErrUnexpectedEOF) is false", i)
		}
		if r != nil {
			t.Fatalf("attempt %d: reader should be nil on error", i)
		}
	}
}

func TestEmptyNonSeekable(t *testing.T) {
	_, err := oggvorbis.NewReader(reader{bytes.NewReader(nil)})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error should be unexpected EOF, but is %v", err)
	}
}

func TestHeaderHelpersOnEmptyInput(t *testing.T) {
	_, err := oggvorbis.GetFormat(bytes.NewReader(nil))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("GetFormat: error should be unexpected EOF, but is %v", err)
	}
	_, err = oggvorbis.GetCommentHeader(bytes.NewReader(nil))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("GetCommentHeader: error should be unexpected EOF, but is %v", err)
	}
	_, _, err = oggvorbis.GetLength(bytes.NewReader(nil))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("GetLength: error should be unexpected EOF, but is %v", err)
	}
}

func TestTruncatedPageHeader(t *testing.T) {
	pages := readTestPages(t)
	first := pages[0]
	for n := 0; n < 27; n++ {
		if n >= len(first) {
			break
		}
		_, err := oggvorbis.NewReader(bytes.NewReader(first[:n]))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("page header truncated to %d bytes: error should be unexpected EOF, but is %v", n, err)
		}
		if err == io.EOF {
			t.Fatalf("page header truncated to %d bytes: returned bare EOF", n)
		}
	}
}

func TestTruncatedSegmentTable(t *testing.T) {
	pages := readTestPages(t)
	// The second page has 16 segments, so a partial table is easy to build.
	page := pages[1]
	const headerSize = 27
	segments := int(page[26])
	if segments == 0 {
		t.Fatal("test page unexpectedly has no segments")
	}
	for n := 0; n < segments; n++ {
		data := page[:headerSize+n]
		_, err := oggvorbis.NewReader(bytes.NewReader(data))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("segment table truncated to %d/%d bytes: error should be unexpected EOF, but is %v", n, segments, err)
		}
	}
}

func TestTruncatedPageContent(t *testing.T) {
	pages := readTestPages(t)
	page := pages[1]
	const headerSize = 27
	segments := int(page[26])
	contentStart := headerSize + segments
	contentSize := len(page) - contentStart
	if contentSize == 0 {
		t.Fatal("test page unexpectedly has no content")
	}
	for _, n := range []int{0, 1, contentSize / 2, contentSize - 1} {
		data := page[:contentStart+n]
		_, err := oggvorbis.NewReader(bytes.NewReader(data))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("content truncated to %d/%d bytes: error should be unexpected EOF, but is %v", n, contentSize, err)
		}
	}
}

func TestTruncatedIsRepeatable(t *testing.T) {
	pages := readTestPages(t)
	data := pages[1][:len(pages[1])-1]
	wrappers := map[string]func() io.Reader{
		"seekable":     func() io.Reader { return bytes.NewReader(data) },
		"non-seekable": func() io.Reader { return reader{bytes.NewReader(data)} },
	}
	for name, newInput := range wrappers {
		_, first := oggvorbis.NewReader(newInput())
		if !errors.Is(first, io.ErrUnexpectedEOF) {
			t.Fatalf("%s: first error should be unexpected EOF, but is %v", name, first)
		}
		for i := 0; i < 3; i++ {
			_, err := oggvorbis.NewReader(newInput())
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("%s attempt %d: error should be unexpected EOF, but is %v", name, i, err)
			}
			if err != first {
				t.Fatalf("%s attempt %d: error %v differs from previous %v", name, i, err, first)
			}
		}
	}
}

func TestCompletePage(t *testing.T) {
	pages := readTestPages(t)
	// A complete first page contains the whole identification header, so
	// GetFormat can parse it; one byte less must be reported as truncation.
	format, err := oggvorbis.GetFormat(bytes.NewReader(pages[0]))
	if err != nil {
		t.Fatalf("complete page: unexpected error %v", err)
	}
	if format.SampleRate != 44100 {
		t.Errorf("complete page: sample rate is %d, expected %d", format.SampleRate, 44100)
	}
	_, err = oggvorbis.GetFormat(bytes.NewReader(pages[0][:len(pages[0])-1]))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial page: error should be unexpected EOF, but is %v", err)
	}
}

func TestChecksumErrorUnchanged(t *testing.T) {
	pages := readTestPages(t)
	page := append([]byte(nil), pages[0]...)
	// Corrupt the last content byte; the checksum must still be validated
	// and reported as its own error, not as truncation.
	page[len(page)-1] ^= 0xFF
	_, err := oggvorbis.GetFormat(bytes.NewReader(page))
	if err == nil {
		t.Fatal("corrupted page: expected an error")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || err == io.EOF {
		t.Fatalf("corrupted page: error should not be an EOF, but is %v", err)
	}
	if err.Error() != "ogg: wrong checksum" {
		t.Fatalf("corrupted page: error is %q, expected %q", err, "ogg: wrong checksum")
	}
}

func TestReadEndsWithEOF(t *testing.T) {
	ogg, err := os.Open("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	defer ogg.Close()

	_, _, err = oggvorbis.ReadAll(reader{ogg})
	if err != nil {
		t.Fatalf("decoding error: %v", err)
	}

	ogg.Seek(0, io.SeekStart)
	r, err := oggvorbis.NewReader(ogg)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 4096)
	calls := 0
	for {
		calls++
		_, err := r.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error after %d reads: %v", calls, err)
		}
		if calls > 10000 {
			t.Fatal("too many reads without EOF")
		}
	}
}

func TestStreamWithoutEndOfStreamFlag(t *testing.T) {
	// testdata/eof_issue.ogg has no EndOfStream flag on its final page.
	// Reaching the end of the input at a page boundary must still be a
	// clean io.EOF, not a truncation error.
	for _, wrap := range []bool{false, true} {
		ogg, err := os.Open("testdata/eof_issue.ogg")
		if err != nil {
			t.Fatal(err)
		}
		var in io.Reader = ogg
		if wrap {
			in = reader{ogg}
		}
		data, _, err := oggvorbis.ReadAll(in)
		ogg.Close()
		if err != nil {
			t.Fatalf("wrap=%v: decoding error: %v", wrap, err)
		}
		if len(data) == 0 {
			t.Fatalf("wrap=%v: no samples decoded", wrap)
		}
	}
}

// readTestPages returns the individual raw Ogg pages of testdata/test.ogg.
func readTestPages(t *testing.T) [][]byte {
	t.Helper()
	data, err := ioutil.ReadFile("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	var pages [][]byte
	for offset := 0; offset < len(data); {
		if len(data)-offset < 27 || string(data[offset:offset+4]) != "OggS" {
			t.Fatalf("no OggS capture pattern at offset %d", offset)
		}
		segments := int(data[offset+26])
		if len(data)-offset < 27+segments {
			t.Fatalf("truncated segment table at offset %d", offset)
		}
		size := 27 + segments
		for _, s := range data[offset+27 : offset+27+segments] {
			size += int(s)
		}
		if len(data)-offset < size {
			t.Fatalf("truncated page content at offset %d", offset)
		}
		pages = append(pages, data[offset:offset+size])
		offset += size
	}
	if len(pages) < 2 {
		t.Fatalf("expected at least 2 pages, got %d", len(pages))
	}
	return pages
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
