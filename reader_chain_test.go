package oggvorbis_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jfreymuth/oggvorbis"
)

const (
	seg1Samples = 44100  // test.ogg
	seg2Samples = 882000 // long.ogg
)

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// chainedFixture returns the concatenation of test.ogg and long.ogg: two
// complete vorbis streams of different lengths (and different serial
// numbers) chained into one file.
func chainedFixture(t *testing.T) []byte {
	t.Helper()
	return append(readFile(t, "testdata/test.ogg"), readFile(t, "testdata/long.ogg")...)
}

// chainedReference decodes both segments individually and returns the
// concatenated PCM data.
func chainedReference(t *testing.T) []float32 {
	t.Helper()
	var ref []float32
	for _, name := range []string{"testdata/test.ogg", "testdata/long.ogg"} {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		data, _, err := oggvorbis.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		ref = append(ref, data...)
	}
	if len(ref) != seg1Samples+seg2Samples {
		t.Fatalf("reference has %d samples, expected %d", len(ref), seg1Samples+seg2Samples)
	}
	return ref
}

// readEverything reads from r until an error occurs and returns all samples.
func readEverything(r *oggvorbis.Reader) ([]float32, error) {
	buf := make([]float32, 4096)
	var out []float32
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
		if n == 0 {
			return out, io.ErrNoProgress
		}
	}
}

func testChainedRead(t *testing.T, wrap bool) {
	chained := chainedFixture(t)
	ref := chainedReference(t)

	var in io.Reader = bytes.NewReader(chained)
	if wrap {
		// hide the io.Seeker implementation
		in = reader{in}
	}
	r, err := oggvorbis.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	if !wrap {
		if r.Length() != seg1Samples+seg2Samples {
			t.Fatalf("length is %d, expected %d", r.Length(), seg1Samples+seg2Samples)
		}
	}
	data, err := readEverything(r)
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	compare(t, data, ref, 1e-7)
	if pos := r.Position(); pos != seg1Samples+seg2Samples {
		t.Errorf("position is %d, expected %d", pos, seg1Samples+seg2Samples)
	}
}

func TestChainedRead(t *testing.T) {
	testChainedRead(t, false)
}

func TestChainedReadNonSeekable(t *testing.T) {
	testChainedRead(t, true)
}

// TestChainedSeam checks the sample-exact transition between the two
// segments: after reading exactly the first segment, the position must be
// the segment boundary, and reading must continue without overlapping or
// missing samples.
func TestChainedSeam(t *testing.T) {
	chained := chainedFixture(t)
	ref := chainedReference(t)

	r, err := oggvorbis.NewReader(bytes.NewReader(chained))
	if err != nil {
		t.Fatal(err)
	}
	// read the first segment in small, odd-sized chunks
	got := make([]float32, seg1Samples)
	read := 0
	for read < len(got) {
		n, err := r.Read(got[read:min(read+997, len(got))])
		if err != nil {
			t.Fatal(err)
		}
		read += n
	}
	if pos := r.Position(); pos != seg1Samples {
		t.Fatalf("position at seam is %d, expected %d", pos, seg1Samples)
	}
	compare(t, got, ref[:seg1Samples], 1e-7)

	// read across the seam and through the second segment
	rest, err := readEverything(r)
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	compare(t, rest, ref[seg1Samples:], 1e-7)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestChainedLength(t *testing.T) {
	chained := chainedFixture(t)

	length, format, err := oggvorbis.GetLength(bytes.NewReader(chained))
	if err != nil {
		t.Fatal(err)
	}
	if length != seg1Samples+seg2Samples {
		t.Errorf("length is %d, expected %d", length, seg1Samples+seg2Samples)
	}
	if format.SampleRate != 44100 || format.Channels != 1 {
		t.Errorf("format is %v, expected 44100 Hz, 1 channel", format)
	}
}

func TestChainedSeek(t *testing.T) {
	chained := chainedFixture(t)
	ref := chainedReference(t)

	r, err := oggvorbis.NewReader(bytes.NewReader(chained))
	if err != nil {
		t.Fatal(err)
	}
	if int(r.Length()) != len(ref) {
		t.Fatalf("length is %d, expected %d", r.Length(), len(ref))
	}

	sections := []struct{ start, len int64 }{
		// first segment
		{600000 - seg1Samples, 10000},
		{0, 500},
		// reading across the segment boundary
		{seg1Samples - 100, 200},
		{seg1Samples - 1, 10},
		// exactly on the boundary: start of the second segment
		{seg1Samples, 500},
		{seg1Samples + 1, 500},
		// second segment
		{seg1Samples + 182399, 100},
		{seg1Samples + 182400, 100},
		{seg1Samples + 182401, 100},
		// last sample
		{seg1Samples + seg2Samples - 1, 1},
		// out of bounds, then back into the first segment
		{seg1Samples + seg2Samples, 0},
		{200000, 1000},
		// back into the second segment
		{seg1Samples + 300000, 1000},
	}
test:
	for _, s := range sections {
		err := r.SetPosition(s.start)
		if err != nil {
			t.Error(err)
			continue test
		}
		if pos := r.Position(); pos != s.start {
			t.Errorf("position after SetPosition(%d) is %d", s.start, pos)
		}
		if s.len == 0 {
			continue test
		}
		ref := ref[s.start : s.start+s.len]
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

	r.SetPosition(seg1Samples + seg2Samples)
	_, err = r.Read(nil)
	if err != io.EOF {
		t.Errorf("error should be EOF, but is %s", err)
	}
}

// withSampleRate returns a copy of a vorbis stream whose identification
// header (first packet of the first page) is patched to a different sample
// rate, with the page CRC fixed up accordingly.
func withSampleRate(t *testing.T, data []byte, rate uint32) []byte {
	t.Helper()
	out := append([]byte(nil), data...)
	nseg := int(out[26])
	content := 27 + nseg
	if !bytes.Equal(out[content:content+7], []byte("\x01vorbis")) {
		t.Fatal("first packet is not the vorbis identification header")
	}
	binary.LittleEndian.PutUint32(out[content+12:content+16], rate)
	size := 0
	for _, s := range out[27 : 27+nseg] {
		size += int(s)
	}
	fixCRC(out[:content+size])
	return out
}

var crcTable [256]uint32

func init() {
	for i := range crcTable {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		crcTable[i] = r
	}
}

func fixCRC(page []byte) {
	for i := 22; i < 26; i++ {
		page[i] = 0
	}
	var crc uint32
	for _, b := range page {
		crc = crc<<8 ^ crcTable[byte(crc>>24)^b]
	}
	binary.LittleEndian.PutUint32(page[22:26], crc)
}

// splitPages splits an ogg file into its pages.
func splitPages(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var pages [][]byte
	for len(data) > 0 {
		if !bytes.Equal(data[:4], []byte("OggS")) {
			t.Fatal("missing capture pattern")
		}
		nseg := int(data[26])
		size := 27 + nseg
		for _, s := range data[27 : 27+nseg] {
			size += int(s)
		}
		pages = append(pages, data[:size])
		data = data[size:]
	}
	return pages
}

func TestChainedFormatMismatch(t *testing.T) {
	seg1 := readFile(t, "testdata/test.ogg")
	seg2 := withSampleRate(t, readFile(t, "testdata/long.ogg"), 48000)
	chained := append(seg1, seg2...)

	r, err := oggvorbis.NewReader(reader{bytes.NewReader(chained)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = readEverything(r)
	if err == nil || !strings.Contains(err.Error(), "different format") {
		t.Fatalf("expected format mismatch error, got %v", err)
	}
}

func TestChainedSameSerial(t *testing.T) {
	// chaining requires distinct serial numbers; concatenating a file with
	// itself must produce a clear error, not silent EOF
	seg := readFile(t, "testdata/test.ogg")
	chained := append(seg, seg...)

	// seekable: the error is detected when scanning the file
	_, err := oggvorbis.NewReader(bytes.NewReader(chained))
	if err == nil || !strings.Contains(err.Error(), "after end of stream") {
		t.Errorf("expected serial error, got %v", err)
	}

	// non-seekable: the error surfaces while reading
	r, err := oggvorbis.NewReader(reader{bytes.NewReader(chained)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = readEverything(r)
	if err == nil || !strings.Contains(err.Error(), "after end of stream") {
		t.Errorf("expected serial error, got %v", err)
	}
}

func TestChainedMultiplexed(t *testing.T) {
	// interleave the pages of two streams: pages with a foreign serial in
	// the middle of a logical stream are not a valid chain
	pages1 := splitPages(t, readFile(t, "testdata/test.ogg"))
	pages2 := splitPages(t, readFile(t, "testdata/long.ogg"))
	var mux []byte
	for i := 0; i < len(pages1) || i < len(pages2); i++ {
		if i < len(pages1) {
			mux = append(mux, pages1[i]...)
		}
		if i < len(pages2) {
			mux = append(mux, pages2[i]...)
		}
	}

	_, err := oggvorbis.NewReader(bytes.NewReader(mux))
	if err == nil || !strings.Contains(err.Error(), "multiplexed") {
		t.Errorf("expected multiplexed error, got %v", err)
	}

	_, err = oggvorbis.NewReader(reader{bytes.NewReader(mux)})
	if err == nil || !strings.Contains(err.Error(), "multiplexed") {
		t.Errorf("expected multiplexed error, got %v", err)
	}
}

func TestChainedTruncated(t *testing.T) {
	chained := chainedFixture(t)
	// cut the file in the middle of the second segment's header pages
	truncated := chained[:len(readFile(t, "testdata/test.ogg"))+200]

	test := func(in io.Reader) {
		r, err := oggvorbis.NewReader(in)
		if err != nil {
			t.Fatal(err)
		}
		_, err = readEverything(r)
		if err != io.ErrUnexpectedEOF {
			t.Errorf("error should be unexpected EOF, but is %v", err)
		}
	}
	// seekable
	test(bytes.NewReader(truncated))
	// non-seekable
	test(reader{bytes.NewReader(truncated)})
}

func TestChainedCorruptPage(t *testing.T) {
	chained := chainedFixture(t)
	// corrupt a byte inside the second segment's audio data
	off := len(readFile(t, "testdata/test.ogg")) + 5000
	chained[off] ^= 0xFF

	r, err := oggvorbis.NewReader(bytes.NewReader(chained))
	if err != nil {
		t.Fatal(err)
	}
	_, err = readEverything(r)
	if err == nil || err == io.EOF {
		t.Fatalf("expected checksum error, got %v", err)
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum error, got %v", err)
	}
}
