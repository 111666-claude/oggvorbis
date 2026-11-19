package oggvorbis

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/jfreymuth/vorbis"
)

// readPackets splits an ogg stream into its packets.
func readPackets(t *testing.T, data []byte) [][]byte {
	t.Helper()
	r := &oggReader{source: bytes.NewReader(data)}
	var pkts [][]byte
	for {
		p, err := r.NextPacket()
		if err != nil {
			break
		}
		cp := make([]byte, len(p))
		copy(cp, p)
		pkts = append(pkts, cp)
	}
	if len(pkts) < 4 {
		t.Fatalf("stream has only %d packets", len(pkts))
	}
	return pkts
}

// decodeGranules decodes the stream packet by packet and returns the
// cumulative sample position (per channel) after each packet.
func decodeGranules(t *testing.T, pkts [][]byte) (gran []int64, channels int) {
	t.Helper()
	var dec vorbis.Decoder
	for i := 0; i < 3; i++ {
		if err := dec.ReadHeader(pkts[i]); err != nil {
			t.Fatal(err)
		}
	}
	channels = dec.Channels()
	buf := make([]float32, dec.BufferSize())
	gran = make([]int64, len(pkts))
	var cum int64
	for i := 3; i < len(pkts); i++ {
		out, err := dec.DecodeInto(pkts[i], buf)
		if err != nil {
			t.Fatalf("decoding packet %d: %s", i, err)
		}
		cum += int64(len(out) / channels)
		gran[i] = cum
	}
	return gran, channels
}

// buildPage assembles a single ogg page with a correct CRC. If lastContinues
// is set, the last fragment is left unterminated (its size must be a multiple
// of 255) so the packet continues on the next page.
func buildPage(serial, seq uint32, flags byte, granule int64, frags [][]byte, lastContinues bool) []byte {
	var segs []byte
	var content []byte
	for i, f := range frags {
		content = append(content, f...)
		for j := 0; j < len(f)/255; j++ {
			segs = append(segs, 0xFF)
		}
		if i == len(frags)-1 && lastContinues {
			if len(f)%255 != 0 {
				panic("continued fragment must be a multiple of 255 bytes")
			}
		} else {
			segs = append(segs, byte(len(f)%255))
		}
	}
	if len(segs) > 255 {
		panic("too many segments")
	}
	page := make([]byte, 0, 27+len(segs)+len(content))
	page = append(page, 'O', 'g', 'g', 'S', 0, flags)
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], uint64(granule))
	page = append(page, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:4], serial)
	page = append(page, tmp[:4]...)
	binary.LittleEndian.PutUint32(tmp[:4], seq)
	page = append(page, tmp[:4]...)
	page = append(page, 0, 0, 0, 0) // CRC placeholder
	page = append(page, byte(len(segs)))
	page = append(page, segs...)
	page = append(page, content...)
	binary.LittleEndian.PutUint32(page[22:26], crcUpdate(0, page))
	return page
}

// initialOffset returns the difference between the decoded sample count and
// the granule positions used by the encoder: the initial offset trimmed at
// the start of the stream (see Vorbis I spec A.2).
func initialOffset(t *testing.T, data []byte, gran []int64) int64 {
	t.Helper()
	off := 0
	packets := 0
	for {
		if off+27 > len(data) || string(data[off:off+4]) != "OggS" {
			t.Fatal("bad fixture")
		}
		g := int64(binary.LittleEndian.Uint64(data[off+6 : off+14]))
		nseg := int(data[off+26])
		size := 27 + nseg
		m := 0
		for j := 0; j < nseg; j++ {
			s := int(data[off+27+j])
			size += s
			if s < 255 {
				m++
			}
		}
		if g > 0 {
			if packets != 3 {
				t.Fatalf("expected 3 header packets, got %d", packets)
			}
			return gran[3+m-1] - g
		}
		packets += m
		off += size
	}
}

// lastPageGranule returns the granule position of the last page of the
// stream, which may be lower than the decoded sample count when the encoder
// trimmed the stream end.
func lastPageGranule(t *testing.T, data []byte) int64 {
	t.Helper()
	off := 0
	var g int64
	for off+27 <= len(data) {
		if string(data[off:off+4]) != "OggS" {
			t.Fatal("bad fixture")
		}
		g = int64(binary.LittleEndian.Uint64(data[off+6 : off+14]))
		nseg := int(data[off+26])
		size := 27 + nseg
		for j := 0; j < nseg; j++ {
			size += int(data[off+27+j])
		}
		off += size
	}
	return g
}

// repaginate rebuilds an ogg stream from its packets, emitting perPage audio
// packets per page. The audio packet with index split (relative to the first
// audio packet, must be the first packet of a page and larger than 255
// bytes) is split across two pages to create a continued packet. endGranule
// is the granule position of the final (EOS) page and may be lower than the
// decoded sample count (end trimming).
func repaginate(t *testing.T, pkts [][]byte, gran []int64, perPage, split int, endGranule int64) []byte {
	t.Helper()
	const serial = 1234567
	audio := pkts[3:]
	audioGran := gran[3:]
	if split < perPage || split%perPage != 0 || split+perPage > len(audio) {
		t.Fatalf("bad split index %d", split)
	}
	if len(audio[split]) <= 255 {
		t.Fatalf("split packet too small: %d bytes", len(audio[split]))
	}
	type pagePlan struct {
		flags     byte
		granule   int64
		frags     [][]byte
		continues bool
	}
	var plans []pagePlan
	for i := 0; i < len(audio); i += perPage {
		end := i + perPage
		if end > len(audio) {
			end = len(audio)
		}
		p := pagePlan{granule: audioGran[end-1]}
		p.frags = append(p.frags, audio[i:end]...)
		plans = append(plans, p)
	}
	// Split the first packet of page k: append a 255-byte fragment to the
	// previous page (its granule already refers to its last complete
	// packet) and start page k continued with the remainder.
	k := split / perPage
	plans[k-1].frags = append(plans[k-1].frags, audio[split][:255])
	plans[k-1].continues = true
	plans[k].flags |= headerFlagContinuedPacket
	plans[k].frags[0] = audio[split][255:]
	plans[len(plans)-1].flags |= headerFlagEndOfStream
	plans[len(plans)-1].granule = endGranule

	var out []byte
	seq := uint32(0)
	emit := func(flags byte, g int64, frags [][]byte, cont bool) {
		out = append(out, buildPage(serial, seq, flags, g, frags, cont)...)
		seq++
	}
	emit(headerFlagBeginningOfStream, 0, pkts[:1], false)
	emit(0, 0, pkts[1:3], false)
	for _, p := range plans {
		emit(p.flags, p.granule, p.frags, p.continues)
	}
	return out
}

func readSamples(t *testing.T, r *Reader, n int) []float32 {
	t.Helper()
	buf := make([]float32, n)
	got := 0
	for got < n {
		m, err := r.Read(buf[got:])
		got += m
		if err != nil {
			t.Fatalf("read after %d/%d values: %s", got, n, err)
		}
		if m == 0 {
			t.Fatalf("no progress after %d/%d values", got, n)
		}
	}
	return buf
}

func comparePCM(t *testing.T, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length %d, expected %d", len(got), len(want))
	}
	for i := range got {
		d := got[i] - want[i]
		if d > 0.00001 || d < -0.00001 {
			t.Fatalf("different values at index %d (%g != %g)", i, got[i], want[i])
		}
	}
}

// TestSeekIndexContinuedPacket builds a fixed-length fixture whose pages are
// re-emitted with one audio packet split across two pages (a continued
// packet), then checks that seeking around the split and onto the last page
// loses no samples.
func TestSeekIndexContinuedPacket(t *testing.T) {
	data, err := os.ReadFile("testdata/seek_fixture.ogg")
	if err != nil {
		t.Fatal(err)
	}
	pkts := readPackets(t, data)
	gran, ch := decodeGranules(t, pkts)
	// Express granule positions in the encoder's coordinate system so the
	// re-paginated stream trims the same initial offset as the original.
	offset := initialOffset(t, data, gran)
	for i := 3; i < len(gran); i++ {
		gran[i] -= offset
	}

	const perPage = 4
	split := -1
	for i := perPage; i+perPage < len(pkts)-3; i += perPage {
		if len(pkts[3+i]) > 255 {
			split = i
			break
		}
	}
	if split < 0 {
		t.Fatal("no audio packet large enough to split")
	}
	end := lastPageGranule(t, data)
	fixture := repaginate(t, pkts, gran, perPage, split, end)

	// The re-paginated stream must decode to the same PCM as the original.
	want, _, err := ReadAll(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := ReadAll(bytes.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	comparePCM(t, ref, want)

	total := end
	if int64(len(ref)) != total*int64(ch) {
		t.Fatalf("decoded %d values, expected %d", len(ref), total*int64(ch))
	}

	r, err := NewReaderWithSeekIndex(bytes.NewReader(fixture), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if r.r.index == nil {
		t.Fatal("index was not built")
	}
	if n, bound := len(r.r.index.entries), total/5000+1; int64(n) > bound {
		t.Fatalf("index has %d entries, bound is %d", n, bound)
	}

	gBefore := gran[3+split-1] // granule of the page ending in a fragment
	gSplit := gran[3+split]    // position where the split packet completes
	positions := []int64{
		0, 1,
		gBefore - 2, gBefore, gBefore + 1, // decode across the continued packet
		gSplit - 1, gSplit, gSplit + 1,
		gran[3+split+perPage] + 7,
		total/2 + 13,
		total - 700, total - 1, // tail page
	}
	for _, pos := range positions {
		if err := r.SetPosition(pos); err != nil {
			t.Fatalf("SetPosition(%d): %s", pos, err)
		}
		if p := r.Position(); p != pos {
			t.Fatalf("position after SetPosition(%d) is %d", pos, p)
		}
		n := int64(600)
		if pos+n > total {
			n = total - pos
		}
		got := readSamples(t, r, int(n)*ch)
		comparePCM(t, got, ref[pos*int64(ch):(pos+n)*int64(ch)])
	}

	// Reading past the end of the tail page must report EOF without
	// losing or inventing samples.
	if err := r.SetPosition(total - 100); err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 200*ch)
	n := 0
	var readErr error
	for {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			readErr = err
			break
		}
	}
	if readErr != io.EOF {
		t.Fatalf("error should be EOF, but is %v", readErr)
	}
	comparePCM(t, buf[:n], ref[(total-100)*int64(ch):])
}

// TestSeekIndexScanCount checks that an indexed seek inspects only the pages
// between the nearest anchor and the target instead of rescanning the whole
// stream.
func TestSeekIndexScanCount(t *testing.T) {
	data, err := os.ReadFile("testdata/long.ogg")
	if err != nil {
		t.Fatal(err)
	}
	const pos = 800000

	plain, err := NewReaderWithSeekIndex(bytes.NewReader(data), -1)
	if err != nil {
		t.Fatal(err)
	}
	if plain.r.index != nil {
		t.Fatal("index should be disabled for interval <= 0")
	}
	if err := plain.SetPosition(pos); err != nil {
		t.Fatal(err)
	}
	scansPlain := plain.r.seekScans

	indexed, err := NewReaderWithSeekIndex(bytes.NewReader(data), 120000)
	if err != nil {
		t.Fatal(err)
	}
	if indexed.r.index == nil {
		t.Fatal("index was not built")
	}
	if err := indexed.SetPosition(pos); err != nil {
		t.Fatal(err)
	}
	scansIndexed := indexed.r.seekScans

	t.Logf("pages scanned: without index %d, with index %d", scansPlain, scansIndexed)
	if scansPlain < 10 {
		t.Fatalf("unindexed seek scanned only %d pages, expected a full scan", scansPlain)
	}
	if scansIndexed > 4 {
		t.Fatalf("indexed seek scanned %d pages, expected at most 4", scansIndexed)
	}

	// Index memory stays bounded by length/interval+1 entries no matter
	// how often we seek.
	for p := int64(0); p < 882000; p += 61333 {
		if err := indexed.SetPosition(p); err != nil {
			t.Fatal(err)
		}
	}
	if n, bound := len(indexed.r.index.entries), 882000/120000+1; n > bound {
		t.Fatalf("index grew to %d entries, bound is %d", n, bound)
	}
}

// corruptGranule returns a copy of data with the granule position of the
// pageIdx-th page replaced (the page CRC is fixed up so the page itself
// stays readable).
func corruptGranule(t *testing.T, data []byte, pageIdx int, newGranule int64) []byte {
	t.Helper()
	out := append([]byte(nil), data...)
	off := 0
	for i := 0; ; i++ {
		if off+27 > len(out) || string(out[off:off+4]) != "OggS" {
			t.Fatal("bad fixture")
		}
		nseg := int(out[off+26])
		size := 27 + nseg
		for j := 0; j < nseg; j++ {
			size += int(out[off+27+j])
		}
		if i == pageIdx {
			binary.LittleEndian.PutUint64(out[off+6:off+14], uint64(newGranule))
			copy(out[off+22:off+26], []byte{0, 0, 0, 0})
			binary.LittleEndian.PutUint32(out[off+22:off+26], crcUpdate(0, out[off:off+size]))
			return out
		}
		off += size
	}
}

// TestSeekIndexBadGranule checks that a stream with non-monotonic granule
// positions disables the index and produces a clear error from SetPosition
// instead of seeking to a wrong position.
func TestSeekIndexBadGranule(t *testing.T) {
	data, err := os.ReadFile("testdata/long.ogg")
	if err != nil {
		t.Fatal(err)
	}
	// Page 5 of long.ogg has granule 243840; give it a granule lower than
	// the previous page's (182400).
	bad := corruptGranule(t, data, 5, 5)

	r, err := NewReaderWithSeekIndex(bytes.NewReader(bad), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if r.r.index != nil {
		t.Fatal("index should be disabled for bad granule positions")
	}
	// Decoding from the start is unaffected.
	pcm := readSamples(t, r, 1000)
	if len(pcm) != 1000 {
		t.Fatalf("read %d values", len(pcm))
	}
	// A seek that stays before the corrupt page still works.
	if err := r.SetPosition(10000); err != nil {
		t.Fatalf("seek before corrupt page: %s", err)
	}
	// A seek that would have to scan past it fails loudly.
	if err := r.SetPosition(400000); !errors.Is(err, errBadGranule) {
		t.Fatalf("error should be errBadGranule, but is %v", err)
	}
}

// TestSeekIndexChainedStream checks that chained streams (granule positions
// restart per chain) disable the index and degrade gracefully: seeking
// inside the first chain keeps working, seeking past it ends at EOF.
func TestSeekIndexChainedStream(t *testing.T) {
	data, err := os.ReadFile("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	chained := append(append([]byte(nil), data...), data...)
	ref, _, err := ReadAll(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	r, err := NewReaderWithSeekIndex(bytes.NewReader(chained), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if r.r.index != nil {
		t.Fatal("index should be disabled for chained streams")
	}
	if err := r.SetPosition(100); err != nil {
		t.Fatalf("seek within first chain: %s", err)
	}
	comparePCM(t, readSamples(t, r, 500), ref[100:600])
	// The reported length covers only the first chain; seeking to it
	// degrades to EOF.
	if err := r.SetPosition(r.Length()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]float32, 100)); err != io.EOF {
		t.Fatalf("error should be EOF, but is %v", err)
	}
}

// resizableReader is an io.ReadSeeker whose underlying data can be truncated
// to simulate a stream that changes after the reader was created.
type resizableReader struct {
	data []byte
	off  int64
}

func (r *resizableReader) Read(p []byte) (int, error) {
	if r.off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += int64(n)
	return n, nil
}

func (r *resizableReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = int64(len(r.data)) + offset
	}
	if abs < 0 {
		return 0, errors.New("negative offset")
	}
	r.off = abs
	return abs, nil
}

// TestSeekIndexStreamChanged checks that when the stream shrinks after the
// index was built, seeks either keep working (anchor still valid) or fail
// with an error instead of returning wrong data or looping forever.
func TestSeekIndexStreamChanged(t *testing.T) {
	data, err := os.ReadFile("testdata/long.ogg")
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := ReadAll(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	rr := &resizableReader{data: data}
	r, err := NewReaderWithSeekIndex(rr, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if r.r.index == nil {
		t.Fatal("index was not built")
	}
	// Truncate the stream to half its size.
	rr.data = rr.data[:len(rr.data)/2]

	// A seek into the surviving half still works and is sample-accurate.
	if err := r.SetPosition(10000); err != nil {
		t.Fatalf("seek into surviving data: %s", err)
	}
	comparePCM(t, readSamples(t, r, 100), ref[10000:10100])

	// A seek into the truncated half must not follow a stale anchor; the
	// index is dropped and the scan reports an error.
	if r.r.index == nil {
		t.Fatal("index dropped too early")
	}
	err = r.SetPosition(800000)
	if err == nil {
		t.Fatal("seek into truncated data should fail")
	}
	if r.r.index != nil {
		t.Fatal("index should be dropped after detecting the changed stream")
	}
}
