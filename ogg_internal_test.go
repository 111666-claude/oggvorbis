package oggvorbis

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"testing"
)

type noSeekReader struct{ io.Reader }

// collectPrefixPackets reads the three vorbis header packets plus all
// packets of the first audio page from an ogg file, and returns them
// together with that page's granule position.
func collectPrefixPackets(t *testing.T, path string) ([][]byte, int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := oggReader{source: bytes.NewReader(data)}
	var packets [][]byte
	for {
		pkt, err := r.NextPacket()
		if err != nil {
			t.Fatalf("reading packet %d: %v", len(packets), err)
		}
		cp := make([]byte, len(pkt))
		copy(cp, pkt)
		packets = append(packets, cp)
		if len(packets) > 3 && r.packetIndex == r.currentPage.packetCount {
			return packets, r.currentPage.AbsoluteGranulePosition
		}
	}
}

type oggPageSpec struct {
	flags     byte
	granule   int64
	continued bool
	packets   [][]byte
	// rawContent, when non-nil, is used as the page content instead of
	// the concatenated packets (used to split a packet across pages).
	rawContent  []byte
	rawSegments []byte
}

// buildOgg assembles pages into a complete ogg stream.
func buildOgg(t *testing.T, pages []oggPageSpec) []byte {
	t.Helper()
	var out []byte
	for i, spec := range pages {
		content := spec.rawContent
		var segments []byte
		if spec.rawSegments != nil {
			segments = spec.rawSegments
		} else {
			for _, pkt := range spec.packets {
				content = append(content, pkt...)
				for s := len(pkt); s >= 255; s -= 255 {
					segments = append(segments, 0xFF)
				}
				segments = append(segments, byte(len(pkt)%255))
			}
		}
		if len(segments) > 255 {
			t.Fatalf("page %d has too many segments: %d", i, len(segments))
		}
		hdr := make([]byte, 27)
		copy(hdr[0:4], "OggS")
		hdr[4] = 0
		hdr[5] = spec.flags
		binary.LittleEndian.PutUint64(hdr[6:14], uint64(spec.granule))
		binary.LittleEndian.PutUint32(hdr[14:18], 1)         // serial
		binary.LittleEndian.PutUint32(hdr[18:22], uint32(i)) // sequence
		hdr[26] = byte(len(segments))
		crc := crcUpdate(0, hdr)
		crc = crcUpdate(crc, segments)
		crc = crcUpdate(crc, content)
		binary.LittleEndian.PutUint32(hdr[22:26], crc)
		out = append(out, hdr...)
		out = append(out, segments...)
		out = append(out, content...)
	}
	return out
}

// readAllSamples decodes a stream completely and returns the samples.
func readAllSamples(t *testing.T, r *Reader) []float32 {
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
			t.Fatalf("read error: %v", err)
		}
		if n == 0 {
			t.Fatal("no progress")
		}
	}
}

// checkSeekableVsNonSeekable verifies that a seekable and a non-seekable
// reader over the same data produce identical headers and samples, and
// that the probed length matches wantLength. If wantSamples is
// non-negative, the decoded sample count must equal it exactly (this is
// not the case when the final page lacks an EndOfStream flag, since the
// reader then cannot know to trim the trailing partial block).
func checkSeekableVsNonSeekable(t *testing.T, data []byte, wantLength, wantSamples int64) {
	t.Helper()

	sr, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader over seekable source: %v", err)
	}
	nr, err := NewReader(noSeekReader{bytes.NewReader(data)})
	if err != nil {
		t.Fatalf("NewReader over non-seekable source: %v", err)
	}

	if sr.SampleRate() != nr.SampleRate() || sr.Channels() != nr.Channels() {
		t.Errorf("format mismatch: seekable %d/%d, non-seekable %d/%d",
			sr.SampleRate(), sr.Channels(), nr.SampleRate(), nr.Channels())
	}
	sc, nc := sr.CommentHeader(), nr.CommentHeader()
	if sc.Vendor != nc.Vendor || len(sc.Comments) != len(nc.Comments) {
		t.Errorf("comment header mismatch: %q %v vs %q %v", sc.Vendor, sc.Comments, nc.Vendor, nc.Comments)
	} else {
		for i := range sc.Comments {
			if sc.Comments[i] != nc.Comments[i] {
				t.Errorf("comment %d mismatch: %q vs %q", i, sc.Comments[i], nc.Comments[i])
			}
		}
	}

	if sr.Length() != wantLength {
		t.Errorf("probed length is %d, expected %d", sr.Length(), wantLength)
	}

	samplesSeekable := readAllSamples(t, sr)
	samplesNonSeekable := readAllSamples(t, nr)
	if len(samplesSeekable) != len(samplesNonSeekable) {
		t.Fatalf("sample count mismatch: seekable %d, non-seekable %d",
			len(samplesSeekable), len(samplesNonSeekable))
	}
	for i := range samplesSeekable {
		if samplesSeekable[i] != samplesNonSeekable[i] {
			t.Fatalf("sample %d mismatch: %g != %g", i, samplesSeekable[i], samplesNonSeekable[i])
		}
	}
	if wantSamples >= 0 && int64(len(samplesSeekable))/int64(sr.Channels()) != wantSamples {
		t.Errorf("decoded %d samples, expected %d",
			len(samplesSeekable)/sr.Channels(), wantSamples)
	}
}

// TestProbeResetsReadStateSinglePage builds a valid single-page vorbis
// stream. Probing its length used to leave stale bytes in the internal
// buffer, so the subsequent header parse read garbage.
func TestProbeResetsReadStateSinglePage(t *testing.T) {
	packets, granule := collectPrefixPackets(t, "testdata/test.ogg")
	data := buildOgg(t, []oggPageSpec{{
		flags:   headerFlagBeginningOfStream | headerFlagEndOfStream,
		granule: granule,
		packets: packets,
	}})
	checkSeekableVsNonSeekable(t, data, granule, granule)
}

// TestProbeResetsReadStateMultiPage builds a two-page stream (headers,
// then audio) with an EndOfStream flag on the last page.
func TestProbeResetsReadStateMultiPage(t *testing.T) {
	packets, granule := collectPrefixPackets(t, "testdata/test.ogg")
	data := buildOgg(t, []oggPageSpec{
		{flags: headerFlagBeginningOfStream, granule: 0, packets: packets[:3]},
		{flags: headerFlagEndOfStream, granule: granule, packets: packets[3:]},
	})
	checkSeekableVsNonSeekable(t, data, granule, granule)
}

// TestProbeResetsReadStateNoEndOfStream builds a two-page stream whose
// last page lacks the EndOfStream flag.
func TestProbeResetsReadStateNoEndOfStream(t *testing.T) {
	packets, granule := collectPrefixPackets(t, "testdata/test.ogg")
	data := buildOgg(t, []oggPageSpec{
		{flags: headerFlagBeginningOfStream, granule: 0, packets: packets[:3]},
		{flags: 0, granule: granule, packets: packets[3:]},
	})
	// Without an EndOfStream flag the trailing partial block cannot be
	// trimmed, so only the probed length is checked.
	checkSeekableVsNonSeekable(t, data, granule, -1)
}

// TestProbeResetsReadStateEmptyLastPage builds a stream whose final page
// contains no packets at all.
func TestProbeResetsReadStateEmptyLastPage(t *testing.T) {
	packets, granule := collectPrefixPackets(t, "testdata/test.ogg")
	data := buildOgg(t, []oggPageSpec{
		{flags: headerFlagBeginningOfStream, granule: 0, packets: packets[:3]},
		{flags: 0, granule: granule, packets: packets[3:]},
		{flags: headerFlagEndOfStream, granule: granule},
	})
	// The empty final page carries the EndOfStream flag but no packet,
	// so the trailing partial block is not trimmed during decoding.
	checkSeekableVsNonSeekable(t, data, granule, -1)
}

// TestContinuedHeaderAcrossPages splits the (large) setup header packet
// across two pages and verifies decoding still works from both seekable
// and non-seekable sources.
func TestContinuedHeaderAcrossPages(t *testing.T) {
	packets, granule := collectPrefixPackets(t, "testdata/test.ogg")
	setup := packets[2]
	if len(setup) < 255*2 {
		t.Fatalf("setup packet too small to split: %d", len(setup))
	}
	split := 255 // split at a segment boundary
	first := setup[:split]
	second := setup[split:]

	firstContent := append(append([]byte{}, packets[0]...), packets[1]...)
	firstContent = append(firstContent, first...)
	var firstSegments []byte
	for _, pkt := range packets[:2] {
		for s := len(pkt); s >= 255; s -= 255 {
			firstSegments = append(firstSegments, 0xFF)
		}
		firstSegments = append(firstSegments, byte(len(pkt)%255))
	}
	for s := 0; s < split; s += 255 {
		firstSegments = append(firstSegments, 0xFF)
	}

	data := buildOgg(t, []oggPageSpec{
		{flags: headerFlagBeginningOfStream, granule: 0,
			rawContent: firstContent, rawSegments: firstSegments},
		{flags: headerFlagContinuedPacket, granule: 0, packets: [][]byte{second}},
		{flags: headerFlagEndOfStream, granule: granule, packets: packets[3:]},
	})
	checkSeekableVsNonSeekable(t, data, granule, granule)
}

// TestShortAndBrokenFiles ensures errors from the probe do not leak
// stale buffer contents into the header parse.
func TestShortAndBrokenFiles(t *testing.T) {
	packets, granule := collectPrefixPackets(t, "testdata/test.ogg")
	good := buildOgg(t, []oggPageSpec{{
		flags:   headerFlagBeginningOfStream | headerFlagEndOfStream,
		granule: granule,
		packets: packets,
	}})

	for _, size := range []int{0, 4, 26, 27, 28, 40, 60, len(good) / 2, len(good) - 1} {
		data := good[:size]
		_, serr := NewReader(bytes.NewReader(data))
		_, nerr := NewReader(noSeekReader{bytes.NewReader(data)})
		if serr == nil {
			t.Errorf("truncated to %d bytes: seekable reader unexpectedly succeeded", size)
		}
		if nerr == nil {
			t.Errorf("truncated to %d bytes: non-seekable reader unexpectedly succeeded", size)
		}
		if serr != nil && nerr != nil && serr.Error() != nerr.Error() {
			t.Errorf("truncated to %d bytes: different errors %q vs %q", size, serr, nerr)
		}
	}
}
