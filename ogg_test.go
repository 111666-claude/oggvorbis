package oggvorbis

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"testing"
)

// buildPage assembles a raw ogg page with a valid checksum.
func buildPage(serial, seq uint32, granule int64, flags byte, segmentTable, content []byte) []byte {
	header := make([]byte, 27)
	copy(header, "OggS")
	header[4] = 0 // stream structure version
	header[5] = flags
	binary.LittleEndian.PutUint64(header[6:], uint64(granule))
	binary.LittleEndian.PutUint32(header[14:], serial)
	binary.LittleEndian.PutUint32(header[18:], seq)
	header[26] = byte(len(segmentTable))
	checksum := crcUpdate(0, header)
	checksum = crcUpdate(checksum, segmentTable)
	checksum = crcUpdate(checksum, content)
	binary.LittleEndian.PutUint32(header[22:], checksum)
	page := append(header, segmentTable...)
	return append(page, content...)
}

func packetsOf(t *testing.T, r *oggReader, n int) [][]byte {
	t.Helper()
	var packets [][]byte
	for i := 0; i < n; i++ {
		p, err := r.NextPacket()
		if err != nil {
			t.Fatalf("packet %d: unexpected error: %v", i, err)
		}
		packets = append(packets, p)
	}
	return packets
}

// Regression test: a page with PageSegments == 0 used to crash with
// "index out of range". It must be read as a complete, empty page after
// which decoding continues with the next page.
func TestZeroSegmentPage(t *testing.T) {
	stream := append([]byte{},
		buildPage(1, 0, 3, headerFlagBeginningOfStream, []byte{3}, []byte("abc"))...)
	stream = append(stream, buildPage(1, 1, -1, 0, nil, nil)...)
	stream = append(stream, buildPage(1, 2, 6, 0, []byte{3}, []byte("def"))...)

	r := &oggReader{source: bytes.NewReader(stream)}
	packets := packetsOf(t, r, 2)
	if string(packets[0]) != "abc" || string(packets[1]) != "def" {
		t.Fatalf("got packets %q, %q; expected %q, %q", packets[0], packets[1], "abc", "def")
	}
	// EOF queries must be idempotent.
	for i := 0; i < 2; i++ {
		if _, err := r.NextPacket(); err != io.EOF {
			t.Fatalf("expected EOF, got %v", err)
		}
	}
}

// A zero-segment page carrying the continued-packet flag must not crash
// or swallow the surrounding packets either.
func TestZeroSegmentPageContinuedFlag(t *testing.T) {
	stream := append([]byte{},
		buildPage(1, 0, 3, 0, []byte{3}, []byte("abc"))...)
	stream = append(stream, buildPage(1, 1, -1, headerFlagContinuedPacket, nil, nil)...)
	stream = append(stream, buildPage(1, 2, 6, 0, []byte{3}, []byte("def"))...)

	r := &oggReader{source: bytes.NewReader(stream)}
	packets := packetsOf(t, r, 2)
	if string(packets[0]) != "abc" || string(packets[1]) != "def" {
		t.Fatalf("got packets %q, %q; expected %q, %q", packets[0], packets[1], "abc", "def")
	}
}

// A single zero-length segment is one empty packet.
func TestZeroLengthSegment(t *testing.T) {
	stream := append([]byte{},
		buildPage(1, 0, 0, 0, []byte{0}, nil)...)
	stream = append(stream, buildPage(1, 1, 3, 0, []byte{3}, []byte("abc"))...)

	r := &oggReader{source: bytes.NewReader(stream)}
	packets := packetsOf(t, r, 2)
	if len(packets[0]) != 0 {
		t.Fatalf("expected empty packet, got %q", packets[0])
	}
	if string(packets[1]) != "abc" {
		t.Fatalf("got packet %q, expected %q", packets[1], "abc")
	}
}

// A page whose last segment is 0xff ends mid-packet; the packet continues
// on the next page.
func TestContinuationAcrossPages(t *testing.T) {
	stream := append([]byte{},
		buildPage(1, 0, -1, 0, []byte{0xff}, bytes.Repeat([]byte("x"), 255))...)
	tail := append(bytes.Repeat([]byte("y"), 255), 'z')
	stream = append(stream, buildPage(1, 1, 256, headerFlagContinuedPacket, []byte{0xff, 1}, tail)...)

	r := &oggReader{source: bytes.NewReader(stream)}
	packets := packetsOf(t, r, 1)
	want := append(bytes.Repeat([]byte("x"), 255), bytes.Repeat([]byte("y"), 255)...)
	want = append(want, 'z')
	if !bytes.Equal(packets[0], want) {
		t.Fatalf("continued packet has length %d, expected %d", len(packets[0]), len(want))
	}
}

func TestTruncatedInput(t *testing.T) {
	full := buildPage(1, 0, 3, 0, []byte{3, 2}, []byte("abcde"))

	cases := []struct {
		name string
		data []byte
		err  error
	}{
		{"truncated header", full[:10], io.ErrUnexpectedEOF},
		{"truncated segment table", full[:27+1], io.ErrUnexpectedEOF},
		{"truncated payload", full[:len(full)-2], io.ErrUnexpectedEOF},
	}
	for _, c := range cases {
		r := &oggReader{source: bytes.NewReader(c.data)}
		if _, err := r.NextPacket(); err != c.err {
			t.Errorf("%s: expected %v, got %v", c.name, c.err, err)
		}
	}

	corrupt := func(name string, mutate func([]byte), msg string) {
		data := append([]byte{}, full...)
		mutate(data)
		r := &oggReader{source: bytes.NewReader(data)}
		if _, err := r.NextPacket(); err == nil || err.Error() != msg {
			t.Errorf("%s: expected %q, got %v", name, msg, err)
		}
	}
	corrupt("bad capture pattern", func(d []byte) { d[0] = 'X' }, "ogg: missing capture pattern")
	corrupt("bad version", func(d []byte) { d[4] = 1 }, "ogg: unsupported version")
	corrupt("bad checksum", func(d []byte) { d[27+2] ^= 0xff }, "ogg: wrong checksum")
}

// Many small (including empty) pages must be processed in one pass each.
func TestManySmallPages(t *testing.T) {
	const pageCount = 6000
	var stream []byte
	want := 0
	for i := 0; i < pageCount; i++ {
		if i%2 == 0 {
			stream = append(stream, buildPage(1, uint32(i), -1, 0, nil, nil)...)
		} else {
			stream = append(stream, buildPage(1, uint32(i), int64(i), 0, []byte{1}, []byte{byte(i)})...)
			want++
		}
	}
	r := &oggReader{source: bytes.NewReader(stream)}
	got := 0
	for {
		_, err := r.NextPacket()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got++
	}
	if got != want {
		t.Fatalf("got %d packets, expected %d", got, want)
	}
}

// splitPages splits a raw ogg stream into its pages.
func splitPages(data []byte) [][]byte {
	var pages [][]byte
	for len(data) > 0 {
		n := int(data[26])
		size := 27 + n
		for _, s := range data[27 : 27+n] {
			size += int(s)
		}
		pages = append(pages, data[:size])
		data = data[size:]
	}
	return pages
}

// Empty pages inserted into a valid stream must not change the decoded
// audio, the reported format/length, or end-of-stream behaviour.
func TestZeroSegmentPageInRealStream(t *testing.T) {
	original, err := os.ReadFile("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	reference, format, err := ReadAll(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}

	pages := splitPages(original)
	serial := binary.LittleEndian.Uint32(pages[0][14:18])
	empty := func(seq uint32) []byte {
		return buildPage(serial, seq, -1, 0, nil, nil)
	}

	var modified []byte
	for i, p := range pages {
		// insert an empty page between the id and comment header pages,
		// and one right before the last (EOS) page
		if i == 1 || i == len(pages)-1 {
			seq := binary.LittleEndian.Uint32(p[18:22])
			modified = append(modified, empty(seq)...)
		}
		modified = append(modified, p...)
	}

	decoded, format2, err := ReadAll(bytes.NewReader(modified))
	if err != nil {
		t.Fatal(err)
	}
	if *format != *format2 {
		t.Fatalf("format changed: %+v vs %+v", format, format2)
	}
	if !equalFloats(decoded, reference) {
		t.Fatalf("decoded audio differs: %d vs %d samples", len(decoded), len(reference))
	}

	// repeated reads after EOF must stay at EOF and produce no samples
	r, err := NewReader(bytes.NewReader(modified))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 4096)
	for {
		_, err := r.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		n, err := r.Read(buf)
		if n != 0 || err != io.EOF {
			t.Fatalf("read after EOF: got (%d, %v), expected (0, EOF)", n, err)
		}
	}
}

func equalFloats(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
