package oggvorbis

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"testing"
)

// --- helpers to rebuild testdata/test.ogg with a custom comment header ---

type oggPage struct {
	header  []byte // 27 bytes, checksum zeroed
	content []byte
	packets [][]byte
}

func parsePages(t *testing.T, data []byte) []oggPage {
	t.Helper()
	var pages []oggPage
	for off := 0; off < len(data); {
		if string(data[off:off+4]) != "OggS" {
			t.Fatalf("missing capture pattern at offset %d", off)
		}
		header := make([]byte, 27)
		copy(header, data[off:off+27])
		nseg := int(header[26])
		segTable := data[off+27 : off+27+nseg]
		total := 0
		var sizes []int
		size := 0
		for _, s := range segTable {
			total += int(s)
			size += int(s)
			if s < 0xFF {
				sizes = append(sizes, size)
				size = 0
			}
		}
		content := data[off+27+nseg : off+27+nseg+total]
		p := oggPage{header: header, content: content}
		coff := 0
		for _, s := range sizes {
			p.packets = append(p.packets, content[coff:coff+s])
			coff += s
		}
		pages = append(pages, p)
		off += 27 + nseg + total
	}
	return pages
}

func lacing(size int) []byte {
	n := size / 255
	out := make([]byte, n+1)
	for i := 0; i < n; i++ {
		out[i] = 0xFF
	}
	out[n] = byte(size % 255)
	return out
}

func buildCommentPacket(vendor string, comments []string) []byte {
	buf := []byte{3}
	buf = append(buf, "vorbis"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(vendor)))
	buf = append(buf, vendor...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(comments)))
	for _, c := range comments {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(c)))
		buf = append(buf, c...)
	}
	buf = append(buf, 1) // framing bit
	return buf
}

func serializePage(p oggPage) []byte {
	var segTable []byte
	var content []byte
	for _, pkt := range p.packets {
		segTable = append(segTable, lacing(len(pkt))...)
		content = append(content, pkt...)
	}
	if len(segTable) > 255 {
		panic("too many segments")
	}
	header := make([]byte, 27)
	copy(header, p.header)
	header[26] = byte(len(segTable))
	// zero checksum before computing
	header[22], header[23], header[24], header[25] = 0, 0, 0, 0
	crc := crcUpdate(0, header)
	crc = crcUpdate(crc, segTable)
	crc = crcUpdate(crc, content)
	binary.LittleEndian.PutUint32(header[22:], crc)
	out := append(header, segTable...)
	return append(out, content...)
}

// oggWithComments returns a copy of testdata/test.ogg whose comment header
// packet is replaced by one carrying the given comments.
func oggWithComments(t *testing.T, comments []string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	pages := parsePages(t, data)
	if len(pages) < 2 || len(pages[1].packets) < 2 {
		t.Fatal("unexpected test.ogg structure")
	}
	// keep the original vendor string
	pkt := pages[1].packets[0]
	if !bytes.HasPrefix(pkt, []byte{3, 'v', 'o', 'r', 'b', 'i', 's'}) {
		t.Fatal("first packet of page 1 is not a comment header")
	}
	vendorLen := int(binary.LittleEndian.Uint32(pkt[7:]))
	vendor := string(pkt[11 : 11+vendorLen])
	pages[1].packets[0] = buildCommentPacket(vendor, comments)

	var out []byte
	for _, p := range pages {
		out = append(out, serializePage(p)...)
	}
	return out
}

func rawReference(t *testing.T) []float32 {
	t.Helper()
	data, err := os.ReadFile("testdata/test.raw")
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]float32, len(data)/4)
	for i := range raw {
		raw[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return raw
}

func readAll(t *testing.T, r *Reader) []float32 {
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
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("no progress")
		}
	}
}

func newRGReader(t *testing.T, comments []string, opts ReplayGain) *Reader {
	t.Helper()
	r, err := NewReader(bytes.NewReader(oggWithComments(t, comments)))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetReplayGain(opts); err != nil {
		t.Fatal(err)
	}
	return r
}

func closeEnough(a, b, tol float32) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < tol
}

// checkGained verifies that got equals raw*gain clipped to +/-limit,
// allowing a small decoding tolerance for unclipped samples while
// requiring clipped samples to sit exactly on the limit.
func checkGained(t *testing.T, got, raw []float32, gain, limit float32) {
	t.Helper()
	if len(got) != len(raw) {
		t.Fatalf("length is %d, expected %d", len(got), len(raw))
	}
	const tol = 1e-4
	clipped := 0
	for i := range got {
		want := raw[i] * gain
		switch {
		case want > limit+tol:
			clipped++
			if got[i] != limit {
				t.Fatalf("sample %d: got %g, want exactly %g", i, got[i], limit)
			}
		case want < -(limit + tol):
			clipped++
			if got[i] != -limit {
				t.Fatalf("sample %d: got %g, want exactly %g", i, got[i], -limit)
			}
		default:
			if want > limit {
				want = limit
			} else if want < -limit {
				want = -limit
			}
			if !closeEnough(got[i], want, tol) {
				t.Fatalf("sample %d: got %g, want %g", i, got[i], want)
			}
		}
	}
	if gain > 1 && clipped == 0 {
		t.Fatal("expected some samples to be clipped")
	}
}

// --- tests ---

func TestReplayGainAttenuation(t *testing.T) {
	raw := rawReference(t)
	r := newRGReader(t, []string{
		"REPLAYGAIN_TRACK_GAIN=-6.0 dB",
		"REPLAYGAIN_TRACK_PEAK=0.98",
	}, ReplayGain{})
	got := readAll(t, r)
	if len(got) != len(raw) {
		t.Fatalf("length is %d, expected %d", len(got), len(raw))
	}
	gain := float32(math.Pow(10, -6.0/20))
	for i := range got {
		want := raw[i] * gain
		if !closeEnough(got[i], want, 1e-4) {
			t.Fatalf("sample %d: got %g, want %g", i, got[i], want)
		}
	}
}

func TestReplayGainAmplificationClips(t *testing.T) {
	raw := rawReference(t)
	gain := float32(math.Pow(10, 6.0/20))
	r := newRGReader(t, []string{
		"REPLAYGAIN_TRACK_GAIN=+6 dB",
	}, ReplayGain{}) // default peak limit 1.0, no peak tag
	got := readAll(t, r)
	checkGained(t, got, raw, gain, 1)
}

func TestReplayGainConfigurablePeakLimit(t *testing.T) {
	raw := rawReference(t)
	const limit = 0.5
	r := newRGReader(t, []string{
		"REPLAYGAIN_TRACK_GAIN=+6 dB",
	}, ReplayGain{PeakLimit: limit})
	got := readAll(t, r)
	checkGained(t, got, raw, float32(math.Pow(10, 6.0/20)), limit)
}

func TestReplayGainPeakTagScaling(t *testing.T) {
	raw := rawReference(t)
	// +6 dB with peak 1.0: gain is reduced so gain*peak == limit (1.0),
	// i.e. the output equals the unmodified samples.
	r := newRGReader(t, []string{
		"REPLAYGAIN_TRACK_GAIN=+6 dB",
		"REPLAYGAIN_TRACK_PEAK=1.0",
	}, ReplayGain{})
	got := readAll(t, r)
	for i := range got {
		if !closeEnough(got[i], raw[i], 1e-4) {
			t.Fatalf("sample %d: got %g, want %g", i, got[i], raw[i])
		}
	}

	// +6 dB with peak 0.5: gain*peak = 0.998 <= 1, no scaling, clipping
	// still applies at the limit.
	r = newRGReader(t, []string{
		"REPLAYGAIN_TRACK_GAIN=+6 dB",
		"REPLAYGAIN_TRACK_PEAK=0.5",
	}, ReplayGain{})
	got = readAll(t, r)
	checkGained(t, got, raw, float32(math.Pow(10, 6.0/20)), 1)
}

func TestReplayGainPreamp(t *testing.T) {
	raw := rawReference(t)
	r := newRGReader(t, []string{
		"REPLAYGAIN_TRACK_GAIN=-6 dB",
	}, ReplayGain{Preamp: 3})
	got := readAll(t, r)
	gain := float32(math.Pow(10, -3.0/20))
	for i := range got {
		if !closeEnough(got[i], raw[i]*gain, 1e-4) {
			t.Fatalf("sample %d: got %g, want %g", i, got[i], raw[i]*gain)
		}
	}
}

func TestReplayGainReadConsistency(t *testing.T) {
	comments := []string{"REPLAYGAIN_TRACK_GAIN=-3 dB"}
	data := oggWithComments(t, comments)

	// reference: one big read with gain enabled
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetReplayGain(ReplayGain{}); err != nil {
		t.Fatal(err)
	}
	ref := readAll(t, r)

	// small odd-sized chunks with gain enabled
	r2, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.SetReplayGain(ReplayGain{}); err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 37)
	var chunked []float32
	for {
		n, err := r2.Read(buf)
		chunked = append(chunked, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(chunked) != len(ref) {
		t.Fatalf("chunked length is %d, expected %d", len(chunked), len(ref))
	}
	for i := range chunked {
		if chunked[i] != ref[i] {
			t.Fatalf("chunked sample %d differs: %g != %g", i, chunked[i], ref[i])
		}
	}

	// returned lengths and Position must match a reader without gain
	r3, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r4, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := r4.SetReplayGain(ReplayGain{}); err != nil {
		t.Fatal(err)
	}
	b3, b4 := make([]float32, 1000), make([]float32, 1000)
	for {
		n3, err3 := r3.Read(b3)
		n4, err4 := r4.Read(b4)
		if n3 != n4 || err3 != err4 || r3.Position() != r4.Position() {
			t.Fatalf("gain changed stream shape: n %d/%d err %v/%v pos %d/%d",
				n3, n4, err3, err4, r3.Position(), r4.Position())
		}
		if err3 == io.EOF {
			break
		}
	}

	// seek with gain enabled matches the corresponding slice of ref
	if err := r.SetPosition(12345); err != nil {
		t.Fatal(err)
	}
	if r.Position() != 12345 {
		t.Fatalf("position after seek is %d, expected 12345", r.Position())
	}
	seekBuf := make([]float32, 1000)
	n := 0
	for n < len(seekBuf) {
		m, err := r.Read(seekBuf[n:])
		if err != nil {
			t.Fatal(err)
		}
		n += m
	}
	for i := range seekBuf {
		if seekBuf[i] != ref[12345+i] {
			t.Fatalf("seek sample %d: got %g, want %g", i, seekBuf[i], ref[12345+i])
		}
	}

	// EOF behavior is unchanged
	if err := r.SetPosition(r.Length()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(seekBuf); err != io.EOF {
		t.Fatalf("error should be EOF, but is %v", err)
	}
}

func TestReplayGainTagRules(t *testing.T) {
	raw := rawReference(t)

	t.Run("missing tags fall back to unity gain", func(t *testing.T) {
		r := newRGReader(t, []string{"ARTIST=someone"}, ReplayGain{})
		got := readAll(t, r)
		for i := range got {
			if !closeEnough(got[i], raw[i], 1e-4) {
				t.Fatalf("sample %d: got %g, want %g", i, got[i], raw[i])
			}
		}
	})

	t.Run("case insensitive key and dB suffix", func(t *testing.T) {
		r := newRGReader(t, []string{"replaygain_track_gain= -6.0 DB "}, ReplayGain{})
		got := readAll(t, r)
		gain := float32(math.Pow(10, -6.0/20))
		for i := range got {
			if !closeEnough(got[i], raw[i]*gain, 1e-4) {
				t.Fatalf("sample %d: got %g, want %g", i, got[i], raw[i]*gain)
			}
		}
	})

	t.Run("duplicate tags: first wins", func(t *testing.T) {
		r := newRGReader(t, []string{
			"REPLAYGAIN_TRACK_GAIN=-6 dB",
			"REPLAYGAIN_TRACK_GAIN=+6 dB",
		}, ReplayGain{})
		got := readAll(t, r)
		gain := float32(math.Pow(10, -6.0/20))
		for i := range got {
			if !closeEnough(got[i], raw[i]*gain, 1e-4) {
				t.Fatalf("sample %d: got %g, want %g", i, got[i], raw[i]*gain)
			}
		}
	})

	t.Run("zero peak is treated as absent", func(t *testing.T) {
		r := newRGReader(t, []string{
			"REPLAYGAIN_TRACK_GAIN=+6 dB",
			"REPLAYGAIN_TRACK_PEAK=0",
		}, ReplayGain{})
		got := readAll(t, r)
		checkGained(t, got, raw, float32(math.Pow(10, 6.0/20)), 1)
	})

	errorCases := []struct {
		name     string
		comments []string
	}{
		{"malformed gain", []string{"REPLAYGAIN_TRACK_GAIN=abc"}},
		{"empty gain", []string{"REPLAYGAIN_TRACK_GAIN="}},
		{"nan gain", []string{"REPLAYGAIN_TRACK_GAIN=nan dB"}},
		{"inf gain", []string{"REPLAYGAIN_TRACK_GAIN=+Inf"}},
		{"malformed first duplicate wins error", []string{
			"REPLAYGAIN_TRACK_GAIN=loud",
			"REPLAYGAIN_TRACK_GAIN=-6 dB",
		}},
		{"malformed peak", []string{"REPLAYGAIN_TRACK_PEAK=1.0.0"}},
		{"nan peak", []string{"REPLAYGAIN_TRACK_PEAK=NaN"}},
		{"inf peak", []string{"REPLAYGAIN_TRACK_PEAK=-inf"}},
	}
	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewReader(bytes.NewReader(oggWithComments(t, tc.comments)))
			if err != nil {
				t.Fatal(err)
			}
			if err := r.SetReplayGain(ReplayGain{}); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}

	t.Run("non-finite options are rejected", func(t *testing.T) {
		r, err := NewReader(bytes.NewReader(oggWithComments(t, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SetReplayGain(ReplayGain{Preamp: math.NaN()}); err == nil {
			t.Fatal("expected error for NaN preamp")
		}
		if err := r.SetReplayGain(ReplayGain{PeakLimit: math.Inf(1)}); err == nil {
			t.Fatal("expected error for infinite peak limit")
		}
	})
}

func TestReplayGainOutputNeverOutOfRange(t *testing.T) {
	r := newRGReader(t, []string{
		"REPLAYGAIN_TRACK_GAIN=+30 dB",
	}, ReplayGain{PeakLimit: 0.75})
	got := readAll(t, r)
	for i, v := range got {
		if v != v {
			t.Fatalf("sample %d is NaN", i)
		}
		if v > 0.75 || v < -0.75 {
			t.Fatalf("sample %d out of range: %g", i, v)
		}
	}
}

func TestReplayGainDisabledByDefault(t *testing.T) {
	// A reader without SetReplayGain must behave exactly as before, even
	// when the file carries ReplayGain tags.
	data := oggWithComments(t, []string{"REPLAYGAIN_TRACK_GAIN=-20 dB"})
	raw := rawReference(t)
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	got := readAll(t, r)
	for i := range got {
		if !closeEnough(got[i], raw[i], 2e-5) {
			t.Fatalf("sample %d: got %g, want %g", i, got[i], raw[i])
		}
	}
}
