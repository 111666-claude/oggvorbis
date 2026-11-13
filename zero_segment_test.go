package oggvorbis

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"testing"
)

// buildPage constructs a syntactically valid Ogg page (correct checksum)
// from the given segment table and payload.
func buildPage(t *testing.T, segments []byte, payload []byte, headerType byte, granule int64, serial, seq uint32) []byte {
	t.Helper()
	hdr := make([]byte, 27)
	copy(hdr[0:4], capturePattern[:])
	hdr[4] = 0 // stream structure version
	hdr[5] = headerType
	binary.LittleEndian.PutUint64(hdr[6:14], uint64(granule))
	binary.LittleEndian.PutUint32(hdr[14:18], serial)
	binary.LittleEndian.PutUint32(hdr[18:22], seq)
	// checksum field at [22:26] stays zero while computing
	hdr[26] = byte(len(segments))
	checksum := crcUpdate(0, hdr)
	checksum = crcUpdate(checksum, segments)
	checksum = crcUpdate(checksum, payload)
	binary.LittleEndian.PutUint32(hdr[22:26], checksum)
	page := make([]byte, 0, 27+len(segments)+len(payload))
	page = append(page, hdr...)
	page = append(page, segments...)
	page = append(page, payload...)
	return page
}

// splitPages splits a byte stream into individual Ogg pages, assuming all
// pages are well-formed.
func splitPages(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var pages [][]byte
	for len(data) > 0 {
		if len(data) < 27 || !bytes.Equal(data[0:4], capturePattern[:]) {
			t.Fatalf("not a page boundary at offset %d", len(pages))
		}
		nSegments := int(data[26])
		tableStart := 27
		payloadSize := 0
		for i := 0; i < nSegments; i++ {
			payloadSize += int(data[tableStart+i])
		}
		pageSize := 27 + nSegments + payloadSize
		if len(data) < pageSize {
			t.Fatalf("truncated page at offset %d", len(pages))
		}
		pages = append(pages, data[:pageSize])
		data = data[pageSize:]
	}
	return pages
}

// A zero-segment page is valid Ogg framing: readHeader must not panic and
// must treat it as a complete, non-continuing page.
func TestZeroSegmentPageRead(t *testing.T) {
	pg := buildPage(t, nil, nil, 0, 42, 1, 0)

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
		t.Errorf("zero-segment page packets: %v, want a single empty trailing slot", p.packets)
	}
}

// Regression for the original crash: a zero-segment page reached through
// NextPacket must be skipped cleanly instead of panicking on the empty
// segment table, and the following page must still decode.
func TestZeroSegmentPageNextPacket(t *testing.T) {
	empty := buildPage(t, nil, nil, 0, 99, 1, 7)
	next := buildPage(t, []byte{3}, []byte{0xAA, 0xBB, 0xCC}, 0, 100, 1, 8)
	r := &oggReader{source: bytes.NewReader(append(empty, next...))}

	packet, err := r.NextPacket()
	if err != nil {
		t.Fatalf("NextPacket across a zero-segment page: %v", err)
	}
	if !bytes.Equal(packet, []byte{0xAA, 0xBB, 0xCC}) {
		t.Errorf("packet after empty page is %x, want aabbcc", packet)
	}
	// The empty page carries no packets and must not update stream state.
	if r.packetIndex != 1 || !r.ready {
		t.Errorf("state after empty page: index=%d ready=%v, want 1/true", r.packetIndex, r.ready)
	}
	if r.currentPage.PageSequenceNumber != 8 {
		t.Errorf("current sequence is %d, want 8 (empty page must not stick)", r.currentPage.PageSequenceNumber)
	}
	if r.currentPage.AbsoluteGranulePosition != 100 {
		t.Errorf("current granule is %d, want 100 (empty page must not pollute the next page's granule)",
			r.currentPage.AbsoluteGranulePosition)
	}
}

// A zero-segment page that also carries the continued-packet header flag
// previously panicked because packetIndex was advanced past the empty
// packets slice. It must instead behave as a page with no packets.
func TestZeroSegmentPageWithContinuationFlag(t *testing.T) {
	empty := buildPage(t, nil, nil, headerFlagContinuedPacket, 0, 1, 0)
	next := buildPage(t, []byte{2}, []byte{0x01, 0x02}, 0, 5, 1, 1)
	r := &oggReader{source: bytes.NewReader(append(empty, next...))}

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

// A page with a single zero-length segment ([0x00]) contains one complete,
// empty packet and does not continue.
func TestSingleZeroLengthSegment(t *testing.T) {
	pg := buildPage(t, []byte{0x00}, nil, 0, 0, 1, 0)

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
	if len(packet) != 0 {
		t.Errorf("empty segment delivered %x, want an empty packet", packet)
	}
}

// A page whose final segment is 0xff is not complete: needsContinue must be
// set, no packet boundary is emitted for the trailing data, and the next
// continuation page completes the packet.
func TestContinuationOnFFSegment(t *testing.T) {
	first := buildPage(t,
		[]byte{0xFF, 0xFF}, // one packet continuing, 510 bytes so far
		bytes.Repeat([]byte{0xAA}, 510), 0, 0, 1, 0)
	second := buildPage(t,
		[]byte{0x02, 0x01}, // finishes the 512-byte packet, then a 1-byte packet
		[]byte{0xBB, 0xBB, 0xCC}, headerFlagContinuedPacket, 10, 1, 1)

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

	secondPacket, err := r.NextPacket()
	if err != nil {
		t.Fatalf("reading packet after continuation: %v", err)
	}
	if !bytes.Equal(secondPacket, []byte{0xCC}) {
		t.Errorf("second packet is %x, want cc", secondPacket)
	}
}

// Truncated inputs must keep producing the existing error semantics:
// io.ErrUnexpectedEOF for cut-off segment tables or payloads, io.EOF only
// for a missing header at a clean stream boundary.
func TestTruncatedInput(t *testing.T) {
	full := buildPage(t, []byte{4}, []byte{1, 2, 3, 4}, 0, 0, 1, 0)

	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"nothing left", nil, io.EOF},
		{"cut-off header", full[:10], io.ErrUnexpectedEOF},
		{"cut-off segment table", full[:27], io.ErrUnexpectedEOF},
		{"cut-off payload", full[:len(full)-2], io.ErrUnexpectedEOF},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p page
			err := p.read(bytes.NewReader(c.data))
			if err != c.want {
				t.Errorf("error is %v, want %v", err, c.want)
			}
		})
	}
}

// A corrupted checksum must still be rejected, including on an empty page:
// the zero-segment fix must not weaken validation of normal or damaged pages.
func TestZeroSegmentPageBadChecksum(t *testing.T) {
	pg := buildPage(t, nil, nil, 0, 0, 1, 0)
	pg[22] ^= 0xFF // flip a checksum byte

	var p page
	err := p.read(bytes.NewReader(pg))
	if err == nil || err.Error() != "ogg: wrong checksum" {
		t.Errorf("corrupted empty page error is %v, want ogg: wrong checksum", err)
	}
}

// A bad capture pattern or unsupported version must still be rejected.
func TestHeaderValidationUnchanged(t *testing.T) {
	good := buildPage(t, nil, nil, 0, 0, 1, 0)

	badPattern := append([]byte{}, good...)
	copy(badPattern[0:4], "XXXX")
	var p page
	if err := p.read(bytes.NewReader(badPattern)); err == nil || err.Error() != "ogg: missing capture pattern" {
		t.Errorf("bad capture pattern error is %v, want missing capture pattern", err)
	}

	badVersion := append([]byte{}, good...)
	badVersion[4] = 1
	if err := p.read(bytes.NewReader(badVersion)); err == nil || err.Error() != "ogg: unsupported version" {
		t.Errorf("bad version error is %v, want unsupported version", err)
	}
}

// Many small pages (including empty ones) must be processed in linear time:
// each page's segment table is scanned exactly once while reading the header.
func TestManySmallPagesLinear(t *testing.T) {
	var stream []byte
	const n = 6000
	for i := 0; i < n; i++ {
		seq := uint32(i)
		if i%2 == 0 {
			stream = append(stream, buildPage(t, nil, nil, 0, int64(i), 1, seq)...)
		} else {
			stream = append(stream, buildPage(t, []byte{1}, []byte{byte(i)}, 0, int64(i), 1, seq)...)
		}
	}
	// close the stream so NextPacket eventually reports EOF
	stream = append(stream, buildPage(t, []byte{0}, nil, headerFlagEndOfStream, int64(n), 1, n)...)

	r := &oggReader{source: bytes.NewReader(stream)}
	delivered := 0
	for {
		packet, err := r.NextPacket()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("page %d: %v", delivered, err)
		}
		if len(packet) == 0 {
			// the closing page's single empty packet is expected once
			if r.lastPacket {
				break
			}
			t.Errorf("unexpected empty packet at iteration %d", delivered)
		}
		delivered++
	}
	if delivered != n/2 {
		t.Errorf("delivered %d packets, want %d", delivered, n/2)
	}
	if !r.lastPacket {
		t.Error("lastPacket should be set after the end-of-stream page")
	}
}

// Repeated reads at EOF must be idempotent, and an empty page must neither
// produce audio samples nor disturb granule/sequence/header state.
func TestEOFIdempotentAndNoEmptySamples(t *testing.T) {
	empty := buildPage(t, nil, nil, 0, 1234, 1, 0)
	last := buildPage(t, []byte{0}, nil, headerFlagEndOfStream, 4321, 1, 1)
	r := &oggReader{source: bytes.NewReader(append(empty, last...))}

	first, err := r.NextPacket()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 0 || !r.lastPacket {
		t.Fatalf("end-of-stream packet: len=%d last=%v", len(first), r.lastPacket)
	}
	// All subsequent reads must return the same EOF, forever.
	for i := 0; i < 3; i++ {
		packet, err := r.NextPacket()
		if err != io.EOF {
			t.Fatalf("repeat %d: err=%v packet=%v, want io.EOF", i, err, packet)
		}
		if packet != nil {
			t.Fatalf("repeat %d: got data %x after EOF", i, packet)
		}
	}
	// State from the empty page must not have overwritten end-of-stream state.
	if r.currentPage.AbsoluteGranulePosition != 4321 {
		t.Errorf("granule is %d, want 4321", r.currentPage.AbsoluteGranulePosition)
	}
	if r.currentPage.PageSequenceNumber != 1 {
		t.Errorf("sequence is %d, want 1", r.currentPage.PageSequenceNumber)
	}
}

// End-to-end: injecting zero-segment pages into a real Vorbis stream must
// not change the decoded samples or the stream's state.
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

	pages := splitPages(t, original)
	var injected []byte
	for i, page := range pages {
		injected = append(injected, page...)
		// After the three header pages, put an empty page between every page.
		if i >= 2 {
			injected = append(injected, buildPage(t, nil, nil, 0, -1,
				binary.LittleEndian.Uint32(page[14:18]),
				binary.LittleEndian.Uint32(page[18:22])+1000+uint32(i))...)
		}
	}

	// Seekable path: LastPosition scans every page, including the empty ones.
	decoded, _, err := ReadAll(bytes.NewReader(injected))
	if err != nil {
		t.Fatalf("decoding seekable stream with empty pages: %v", err)
	}
	compareFloatSlices(t, decoded, expected, 0.00002)

	// Non-seekable path exercises the sequential reader only.
	decoded, _, err = ReadAll(nonSeeker{bytes.NewReader(injected)})
	if err != nil {
		t.Fatalf("decoding non-seekable stream with empty pages: %v", err)
	}
	compareFloatSlices(t, decoded, expected, 0.00002)
}

type nonSeeker struct {
	io.Reader
}

func compareFloatSlices(t *testing.T, got, want []float32, tolerance float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("decoded length is %d, want %d (empty pages must not add samples)", len(got), len(want))
	}
	for i := range got {
		d := got[i] - want[i]
		if d < 0 {
			d = -d
		}
		if d > tolerance {
			t.Fatalf("sample %d differs: %g != %g", i, got[i], want[i])
		}
	}
}
