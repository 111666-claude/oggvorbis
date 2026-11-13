package oggvorbis

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"testing"
)

// buildPage constructs a syntactically valid Ogg page (correct checksum) from
// the given segment table and payload.
func buildPage(segments []byte, payload []byte, headerType byte, granule int64, serial, seq uint32) []byte {
	hdr := make([]byte, 27)
	copy(hdr[0:4], capturePattern[:])
	hdr[4] = 0 // stream structure version
	hdr[5] = headerType
	binary.LittleEndian.PutUint64(hdr[6:14], uint64(granule))
	binary.LittleEndian.PutUint32(hdr[14:18], serial)
	binary.LittleEndian.PutUint32(hdr[18:22], seq)
	// The checksum field at [22:26] stays zero while computing.
	hdr[26] = byte(len(segments))
	checksum := crcUpdate(0, hdr)
	checksum = crcUpdate(checksum, segments)
	checksum = crcUpdate(checksum, payload)
	binary.LittleEndian.PutUint32(hdr[22:26], checksum)

	pg := make([]byte, 0, 27+len(segments)+len(payload))
	pg = append(pg, hdr...)
	pg = append(pg, segments...)
	pg = append(pg, payload...)
	return pg
}

// splitPages splits a byte stream into individual Ogg pages. All pages in the
// input must be well formed.
func splitPages(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var pages [][]byte
	for len(data) > 0 {
		if len(data) < 27 || !bytes.Equal(data[0:4], capturePattern[:]) {
			t.Fatalf("not a page boundary at page %d", len(pages))
		}
		n := int(data[26])
		size := 27 + n
		for i := 0; i < n; i++ {
			size += int(data[27+i])
		}
		if len(data) < size {
			t.Fatalf("truncated page at page %d", len(pages))
		}
		pages = append(pages, data[:size])
		data = data[size:]
	}
	return pages
}

func pageSerial(pg []byte) uint32   { return binary.LittleEndian.Uint32(pg[14:18]) }
func pageSequence(pg []byte) uint32 { return binary.LittleEndian.Uint32(pg[18:22]) }
func pageGranule(pg []byte) int64   { return int64(binary.LittleEndian.Uint64(pg[6:14])) }
func emptyPage(granule int64, seq uint32) []byte {
	return buildPage(nil, nil, 0, granule, 1, seq)
}

// A zero-segment page is valid Ogg framing: reading it must not panic, must
// consume no payload, and must be treated as complete (not continuing).
func TestZeroSegmentPageRead(t *testing.T) {
	pg := buildPage(nil, nil, 0, 42, 1, 0)

	var p page
	if err := p.read(bytes.NewReader(pg)); err != nil {
		t.Fatalf("reading a zero-segment page: %v", err)
	}
	if p.PageSegments != 0 || p.packetCount != 0 || p.totalSize != 0 {
		t.Errorf("zero-segment page: segments=%d packets=%d size=%d, want 0/0/0",
			p.PageSegments, p.packetCount, p.totalSize)
	}
	if p.needsContinue {
		t.Error("zero-segment page must not be marked as needing continuation")
	}
	if len(p.packets) != 1 || len(p.packets[0]) != 0 {
		t.Errorf("zero-segment page packets: %v, want one empty trailing slot", p.packets)
	}
	if p.AbsoluteGranulePosition != 42 || p.PageSequenceNumber != 0 {
		t.Errorf("header fields clobbered: granule=%d seq=%d", p.AbsoluteGranulePosition, p.PageSequenceNumber)
	}
}

// Regression for the original crash: a zero-segment page reached through
// NextPacket must be walked past instead of panicking on the empty segment
// table, and the following page must still decode.
func TestZeroSegmentPageNextPacket(t *testing.T) {
	stream := append(emptyPage(99, 7), buildPage([]byte{3}, []byte{0xAA, 0xBB, 0xCC}, 0, 100, 1, 8)...)
	r := &oggReader{source: bytes.NewReader(stream)}

	packet, err := r.NextPacket()
	if err != nil {
		t.Fatalf("NextPacket across a zero-segment page: %v", err)
	}
	if !bytes.Equal(packet, []byte{0xAA, 0xBB, 0xCC}) {
		t.Errorf("packet after empty page is %x, want aabbcc", packet)
	}
	// The empty page must not leak into the state of the page that carried
	// the packet.
	if r.currentPage.PageSequenceNumber != 8 || r.currentPage.AbsoluteGranulePosition != 100 {
		t.Errorf("current page state is seq=%d granule=%d, want 8/100",
			r.currentPage.PageSequenceNumber, r.currentPage.AbsoluteGranulePosition)
	}
	if r.packetIndex != 1 || !r.ready {
		t.Errorf("state after empty page: index=%d ready=%v, want 1/true", r.packetIndex, r.ready)
	}
}

// A zero-segment page that also carries the continued-packet header flag used
// to advance packetIndex past the empty packets slice.
func TestZeroSegmentPageWithContinuationFlag(t *testing.T) {
	stream := append(buildPage(nil, nil, headerFlagContinuedPacket, 0, 1, 0),
		buildPage([]byte{2}, []byte{0x01, 0x02}, 0, 5, 1, 1)...)
	r := &oggReader{source: bytes.NewReader(stream)}

	packet, err := r.NextPacket()
	if err != nil {
		t.Fatalf("NextPacket across a continued zero-segment page: %v", err)
	}
	if !bytes.Equal(packet, []byte{0x01, 0x02}) {
		t.Errorf("packet after continued empty page is %x, want 0102", packet)
	}
	if r.currentPage.PageSequenceNumber != 1 {
		t.Errorf("current sequence is %d, want 1", r.currentPage.PageSequenceNumber)
	}
}

// Several empty pages in a row are still skipped, and a packet that started
// on an earlier page survives the empty pages.
func TestConsecutiveZeroSegmentPages(t *testing.T) {
	var stream []byte
	stream = append(stream, buildPage([]byte{0xFF}, bytes.Repeat([]byte{0xAA}, 255), 0, 0, 1, 0)...)
	stream = append(stream, buildPage(nil, nil, headerFlagContinuedPacket, 0, 1, 1)...)
	stream = append(stream, buildPage(nil, nil, headerFlagContinuedPacket, 0, 1, 2)...)
	stream = append(stream, buildPage([]byte{2, 1}, []byte{0xBB, 0xBB, 0xCC}, headerFlagContinuedPacket, 10, 1, 3)...)

	r := &oggReader{source: bytes.NewReader(stream)}
	packet, err := r.NextPacket()
	if err != nil {
		t.Fatalf("reassembling across consecutive empty pages: %v", err)
	}
	if len(packet) != 257 || packet[255] != 0xBB || packet[256] != 0xBB {
		t.Fatalf("reassembled packet is %x (len %d), want 255 aa bytes followed by bbbb", packet, len(packet))
	}
	for i := 0; i < 255; i++ {
		if packet[i] != 0xAA {
			t.Fatalf("reassembled packet corrupted at index %d", i)
		}
	}
	next, err := r.NextPacket()
	if err != nil {
		t.Fatalf("packet after reassembly: %v", err)
	}
	if !bytes.Equal(next, []byte{0xCC}) {
		t.Errorf("second packet is %x, want cc", next)
	}
}

// A page with a single zero-length segment ([0x00]) contains one complete,
// empty packet and does not continue.
func TestSingleZeroLengthSegment(t *testing.T) {
	pg := buildPage([]byte{0x00}, nil, 0, 0, 1, 0)

	var p page
	if err := p.read(bytes.NewReader(pg)); err != nil {
		t.Fatalf("reading a [0x00]-segment page: %v", err)
	}
	if p.packetCount != 1 || p.totalSize != 0 || p.needsContinue {
		t.Errorf("[0x00] segment: packets=%d size=%d continue=%v, want 1/0/false",
			p.packetCount, p.totalSize, p.needsContinue)
	}
	if len(p.packets[0]) != 0 || len(p.packets[1]) != 0 {
		t.Errorf("[0x00] segment packets: %v, want one empty complete packet", p.packets)
	}

	r := &oggReader{source: bytes.NewReader(pg)}
	packet, err := r.NextPacket()
	if err != nil {
		t.Fatalf("NextPacket on [0x00]-segment page: %v", err)
	}
	if packet == nil || len(packet) != 0 {
		t.Errorf("empty segment delivered %v, want a non-nil empty packet", packet)
	}
}

// A page whose final segment is 0xff is not complete: needsContinue must be
// set, no packet boundary is emitted for the trailing data, and the next
// continuation page completes the packet.
func TestContinuationOnFFSegment(t *testing.T) {
	first := buildPage([]byte{0xFF, 0xFF}, bytes.Repeat([]byte{0xAA}, 510), 0, 0, 1, 0)
	second := buildPage([]byte{0x02, 0x01}, []byte{0xBB, 0xBB, 0xCC}, headerFlagContinuedPacket, 10, 1, 1)

	var p page
	if err := p.read(bytes.NewReader(first)); err != nil {
		t.Fatalf("reading continuation page: %v", err)
	}
	if !p.needsContinue || p.packetCount != 0 || p.totalSize != 510 {
		t.Errorf("0xff-ending page: continue=%v packets=%d size=%d, want true/0/510",
			p.needsContinue, p.packetCount, p.totalSize)
	}

	r := &oggReader{source: bytes.NewReader(append(first, second...))}
	packet, err := r.NextPacket()
	if err != nil {
		t.Fatalf("reassembling continued packet: %v", err)
	}
	if len(packet) != 512 {
		t.Fatalf("reassembled packet length is %d, want 512", len(packet))
	}
	for i := 0; i < 510; i++ {
		if packet[i] != 0xAA {
			t.Fatalf("reassembled packet corrupted at index %d", i)
		}
	}
	if packet[510] != 0xBB || packet[511] != 0xBB {
		t.Errorf("reassembled packet tail is %x, want bbbb", packet[510:512])
	}

	next, err := r.NextPacket()
	if err != nil {
		t.Fatalf("reading packet after continuation: %v", err)
	}
	if !bytes.Equal(next, []byte{0xCC}) {
		t.Errorf("second packet is %x, want cc", next)
	}
}

// Truncated inputs keep the existing error semantics: io.ErrUnexpectedEOF for
// cut-off headers, segment tables or payloads, and io.EOF only when a read
// starts at a clean stream boundary.
func TestTruncatedInput(t *testing.T) {
	full := buildPage([]byte{4}, []byte{1, 2, 3, 4}, 0, 0, 1, 0)
	noSegments := buildPage(nil, nil, 0, 0, 1, 0)

	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"nothing left", nil, io.EOF},
		{"cut-off header", full[:10], io.ErrUnexpectedEOF},
		{"cut-off segment table", full[:27], io.ErrUnexpectedEOF},
		{"cut-off payload", full[:len(full)-2], io.ErrUnexpectedEOF},
		{"zero segment header alone is complete", noSegments, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p page
			if err := p.read(bytes.NewReader(c.data)); err != c.want {
				t.Errorf("error is %v, want %v", err, c.want)
			}
		})
	}
}

// Validation of damaged pages must be unchanged by the zero-segment fix.
func TestPageValidationUnchanged(t *testing.T) {
	good := buildPage(nil, nil, 0, 0, 1, 0)

	badChecksum := append([]byte{}, good...)
	badChecksum[22] ^= 0xFF

	badPattern := append([]byte{}, good...)
	copy(badPattern[0:4], "XXXX")

	badVersion := append([]byte{}, good...)
	badVersion[4] = 1

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"checksum", badChecksum, "ogg: wrong checksum"},
		{"capture pattern", badPattern, "ogg: missing capture pattern"},
		{"version", badVersion, "ogg: unsupported version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p page
			err := p.read(bytes.NewReader(c.data))
			if err == nil || err.Error() != c.want {
				t.Errorf("error is %v, want %q", err, c.want)
			}
		})
	}

	// The same checksum rule applies to a damaged page with segments.
	damaged := buildPage([]byte{2}, []byte{1, 2}, 0, 0, 1, 0)
	damaged[len(damaged)-1] ^= 0xFF
	var p page
	if err := p.read(bytes.NewReader(damaged)); err == nil || err.Error() != "ogg: wrong checksum" {
		t.Errorf("damaged payload error is %v, want ogg: wrong checksum", err)
	}
}

// Header type flags keep their meaning next to empty pages: continued skips
// the leading partial packet and end-of-stream terminates the stream.
func TestHeaderTypeSemantics(t *testing.T) {
	// continued: when a reader joins a stream mid-packet, the first packet on
	// the first page is a continuation and is skipped.
	continued := buildPage([]byte{1, 1}, []byte{0xBB, 0xCC}, headerFlagContinuedPacket, 1, 1, 1)
	r := &oggReader{source: bytes.NewReader(continued)}
	packet, err := r.NextPacket()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packet, []byte{0xCC}) {
		t.Errorf("packet after continued header is %x, want cc", packet)
	}

	// end of stream: the final packet is flagged, and further reads hit EOF.
	eos := buildPage([]byte{1}, []byte{0xCC}, headerFlagEndOfStream, 5, 1, 0)
	r = &oggReader{source: bytes.NewReader(eos)}
	if _, err := r.NextPacket(); err != nil {
		t.Fatal(err)
	}
	if !r.lastPacket {
		t.Error("end-of-stream flag did not set lastPacket")
	}
	if _, err := r.NextPacket(); err != io.EOF {
		t.Errorf("read after end of stream is %v, want io.EOF", err)
	}
}

// Reads at EOF are idempotent, and empty pages neither add audio samples nor
// disturb granule position or sequence number.
func TestEOFIdempotentAndNoExtraSamples(t *testing.T) {
	stream := append(emptyPage(1234, 0), buildPage([]byte{0}, nil, headerFlagEndOfStream, 4321, 1, 1)...)
	r := &oggReader{source: bytes.NewReader(stream)}

	packet, err := r.NextPacket()
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) != 0 || !r.lastPacket {
		t.Fatalf("end-of-stream packet: len=%d last=%v", len(packet), r.lastPacket)
	}
	for i := 0; i < 3; i++ {
		packet, err := r.NextPacket()
		if err != io.EOF {
			t.Fatalf("repeat %d: err=%v packet=%v, want io.EOF", i, err, packet)
		}
		if packet != nil {
			t.Fatalf("repeat %d: got data %x after EOF", i, packet)
		}
	}
	if r.currentPage.AbsoluteGranulePosition != 4321 {
		t.Errorf("granule is %d, want 4321", r.currentPage.AbsoluteGranulePosition)
	}
	if r.currentPage.PageSequenceNumber != 1 {
		t.Errorf("sequence is %d, want 1", r.currentPage.PageSequenceNumber)
	}
}

// countingReader records how often and how much the source is read.
type countingReader struct {
	r     io.Reader
	reads int
	bytes int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.reads++
	c.bytes += n
	return n, err
}

// 6000 small pages must be handled in linear time: every byte of the stream is
// read exactly once and the segment table is not rescanned per packet.
func TestManySmallPagesLinear(t *testing.T) {
	const n = 6000
	var stream []byte
	nonEmpty := 0
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			stream = append(stream, emptyPage(int64(i), uint32(i))...)
		} else {
			stream = append(stream, buildPage([]byte{1}, []byte{byte(i)}, 0, int64(i), 1, uint32(i))...)
			nonEmpty++
		}
	}
	stream = append(stream, buildPage([]byte{0}, nil, headerFlagEndOfStream, n, 1, n)...)

	src := &countingReader{r: bytes.NewReader(stream)}
	r := &oggReader{source: src}
	delivered := 0
	for {
		packet, err := r.NextPacket()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("packet %d: %v", delivered, err)
		}
		if len(packet) == 0 && !r.lastPacket {
			t.Errorf("unexpected empty packet at iteration %d", delivered)
		}
		delivered++
		if r.lastPacket {
			break
		}
	}
	if delivered != nonEmpty+1 {
		t.Errorf("delivered %d packets, want %d", delivered, nonEmpty+1)
	}
	if src.bytes != len(stream) {
		t.Errorf("read %d bytes from %d-byte stream: some bytes were read more than once",
			src.bytes, len(stream))
	}
	// Two source reads per page at most (header, segment table and payload);
	// a rescan per packet would blow through this linear bound.
	if maxReads := 3 * (n + 1); src.reads > maxReads {
		t.Errorf("source was read %d times for %d pages, want at most %d", src.reads, n+1, maxReads)
	}
}

// End-to-end: zero-segment pages injected throughout a real Vorbis stream must
// not change the decoded samples, length, format or comment header.
func TestZeroSegmentPagesInRealStream(t *testing.T) {
	raw, err := os.ReadFile("testdata/test.raw")
	if err != nil {
		t.Fatal(err)
	}
	expected := make([]float32, len(raw)/4)
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, expected); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	origPages := splitPages(t, original)

	refFormat, err := GetFormat(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	refComment, err := GetCommentHeader(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}

	// Insert an empty page after every page, including the header pages.
	var injected []byte
	for i, pg := range origPages {
		injected = append(injected, pg...)
		injected = append(injected, buildPage(nil, nil, 0, -1,
			pageSerial(pg), pageSequence(pg)+1000+uint32(i))...)
	}

	t.Run("format and comments", func(t *testing.T) {
		f, err := GetFormat(bytes.NewReader(injected))
		if err != nil {
			t.Fatal(err)
		}
		if *f != *refFormat {
			t.Errorf("format is %+v, want %+v", *f, *refFormat)
		}
		c, err := GetCommentHeader(bytes.NewReader(injected))
		if err != nil {
			t.Fatal(err)
		}
		if c.Vendor != refComment.Vendor || len(c.Comments) != len(refComment.Comments) {
			t.Errorf("comment header polluted by empty pages")
		}
	})

	t.Run("seekable", func(t *testing.T) {
		decoded, format, err := ReadAll(bytes.NewReader(injected))
		if err != nil {
			t.Fatalf("decoding seekable stream with empty pages: %v", err)
		}
		compareFloatSlices(t, decoded, expected, 0.00002)
		if format.SampleRate != refFormat.SampleRate || format.Channels != refFormat.Channels {
			t.Errorf("format is %+v, want %+v", *format, *refFormat)
		}
	})

	t.Run("non-seekable", func(t *testing.T) {
		decoded, _, err := ReadAll(nonSeekerReader{bytes.NewReader(injected)})
		if err != nil {
			t.Fatalf("decoding non-seekable stream with empty pages: %v", err)
		}
		compareFloatSlices(t, decoded, expected, 0.00002)
	})

	t.Run("length", func(t *testing.T) {
		length, _, err := GetLength(bytes.NewReader(injected))
		if err != nil {
			t.Fatal(err)
		}
		if length != int64(len(expected)) {
			t.Errorf("length is %d, want %d", length, len(expected))
		}
	})

	// An empty page as the final page must not overwrite the length with its
	// (unknown) granule position of -1. eof_issue.ogg has no end-of-stream
	// flag, so the length comes from the last page the scan can read.
	t.Run("empty page at end of stream", func(t *testing.T) {
		eofIssue, err := os.ReadFile("testdata/eof_issue.ogg")
		if err != nil {
			t.Fatal(err)
		}
		trailing := append(append([]byte{}, eofIssue...),
			buildPage(nil, nil, 0, -1, 1, 9999)...)
		length, _, err := GetLength(bytes.NewReader(trailing))
		if err != nil {
			t.Fatal(err)
		}
		if length != 72384 {
			t.Errorf("length is %d, want 72384", length)
		}
	})
}

type nonSeekerReader struct {
	io.Reader
}

// Seeking must not use an empty page as a resume anchor: an empty page has no
// packet to position on and used to produce a negative packet index.
func TestSeekAcrossZeroSegmentPages(t *testing.T) {
	var stream []byte
	stream = append(stream, buildPage([]byte{1}, []byte{0x01}, 0, 0, 1, 0)...)
	stream = append(stream, emptyPage(100, 1)...)
	stream = append(stream, buildPage([]byte{1}, []byte{0x02}, 0, 200, 1, 2)...)

	src := bytes.NewReader(stream)
	r := &oggReader{source: src, seeker: src}
	if _, err := r.SeekPageBefore(150); err != nil {
		t.Fatalf("SeekPageBefore: %v", err)
	}
	if r.ready && r.packetIndex < 0 {
		t.Fatalf("negative packet index %d after seeking past an empty page", r.packetIndex)
	}
	packet, err := r.NextPacket()
	if err != nil {
		t.Fatalf("reading after seek: %v", err)
	}
	if len(packet) != 1 {
		t.Errorf("packet after seek has length %d, want 1", len(packet))
	}

	// End-to-end with a real stream: empty pages placed at the previous
	// page's granule are the ones that can become the seek anchor.
	original, err := os.ReadFile("testdata/long.ogg")
	if err != nil {
		t.Fatal(err)
	}
	var injected []byte
	for i, pg := range splitPages(t, original) {
		injected = append(injected, pg...)
		injected = append(injected, buildPage(nil, nil, 0, pageGranule(pg),
			pageSerial(pg), pageSequence(pg)+1000+uint32(i))...)
	}
	reference, _, err := ReadAll(nonSeekerReader{bytes.NewReader(injected)})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewReader(bytes.NewReader(injected))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Length() != int64(len(reference)) {
		t.Fatalf("length is %d, want %d", dec.Length(), len(reference))
	}
	for _, pos := range []int64{0, 1, 100, 5000, 182399, 182400, 182401, 200000, 880000} {
		if err := dec.SetPosition(pos); err != nil {
			t.Fatalf("SetPosition(%d): %v", pos, err)
		}
		buf := make([]float32, 256)
		n := 0
		for n < len(buf) {
			m, err := dec.Read(buf[n:])
			n += m
			if err != nil {
				break
			}
		}
		if n > 0 {
			compareFloatSlices(t, buf[:n], reference[pos:pos+int64(n)], 0.00001)
		}
	}
}

func compareFloatSlices(t *testing.T, got, want []float32, tolerance float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("decoded length is %d, want %d (empty pages must not add samples)", len(got), len(want))
	}
	for i := range got {
		if d := float64(got[i]) - float64(want[i]); math.Abs(d) > float64(tolerance) {
			t.Fatalf("sample %d differs: %g != %g", i, got[i], want[i])
		}
	}
}
