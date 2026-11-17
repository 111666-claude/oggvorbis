package oggvorbis_test

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/jfreymuth/oggvorbis"
)

// nonSeeker hides the Seek method so the underlying input only implements
// io.Reader.
type nonSeeker struct{ io.Reader }

func openLoopReader(t *testing.T, name string, seekable bool) *oggvorbis.Reader {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	var in io.Reader = f
	if !seekable {
		in = nonSeeker{f}
	}
	r, err := oggvorbis.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// decodeReference decodes the whole file without looping.
func decodeReference(t *testing.T, name string) []float32 {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _, err := oggvorbis.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoopPointsParsing(t *testing.T) {
	r := openLoopReader(t, "testdata/loop.ogg", true)
	start, end, err := r.LoopPoints()
	if err != nil {
		t.Fatal(err)
	}
	if start != 10000 || end != 40000 {
		t.Fatalf("loop points = (%d,%d), want (10000,40000)", start, end)
	}
	if r.LoopEnabled() {
		t.Fatal("looping must be disabled by default")
	}
}

func TestLoopPointsErrors(t *testing.T) {
	tests := []struct {
		name string
		file string
		want error
	}{
		{"missing tags", "testdata/loop_notags.ogg", oggvorbis.ErrLoopPointsMissing},
		{"duplicate tag", "testdata/loop_dupstart.ogg", oggvorbis.ErrLoopPointsDuplicate},
		{"inverted points", "testdata/loop_inverted.ogg", oggvorbis.ErrLoopPointsInvalid},
		{"out of range", "testdata/loop_outofrange.ogg", oggvorbis.ErrLoopPointsOutOfRange},
		{"non-numeric value", "testdata/loop_badvalue.ogg", oggvorbis.ErrLoopPointsInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := openLoopReader(t, tc.file, true)
			if _, _, err := r.LoopPoints(); !errors.Is(err, tc.want) {
				t.Fatalf("LoopPoints() error = %v, want %v", err, tc.want)
			}
			if err := r.SetLoopFromTags(); !errors.Is(err, tc.want) {
				t.Fatalf("SetLoopFromTags() error = %v, want %v", err, tc.want)
			}
			if r.LoopEnabled() {
				t.Fatal("looping must stay disabled after a failed enable")
			}
		})
	}
}

func TestLoopRequiresSeekableInput(t *testing.T) {
	r := openLoopReader(t, "testdata/loop.ogg", false)
	if err := r.SetLoopFromTags(); err == nil {
		t.Fatal("SetLoopFromTags on a non-seekable reader must fail")
	}
	if r.LoopEnabled() {
		t.Fatal("looping must stay disabled on a non-seekable reader")
	}
	// Plain, linear playback on the non-seekable input still terminates and
	// produces the whole stream: no infinite spinning.
	buf := make([]float32, 4096)
	samples := 0
	for {
		n, err := r.Read(buf)
		samples += n / r.Channels()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("reader made no progress")
		}
	}
	if samples != int(r.Length()) {
		t.Fatalf("decoded %d samples, want %d", samples, r.Length())
	}
}

// frameAt returns the interleaved channel values of frame i from a full
// decoded reference.
func frameAt(ref []float32, ch, i int) []float32 {
	return ref[i*ch : (i+1)*ch]
}

func framesEqual(a, b []float32, tol float32) bool {
	for i := range a {
		d := a[i] - b[i]
		if d < 0 {
			d = -d
		}
		if d > tol {
			return false
		}
	}
	return true
}

// expectedSource maps an output frame index to the frame index in the
// unlooped reference for an infinite loop over [start,end) with a linear
// lead-in before start.
func infiniteSource(i, start, span int64) int64 {
	if i < start {
		return i
	}
	return start + (i-start)%span
}

// TestInfiniteLoopWaveform verifies the emitted PCM across many wrap points
// against the unlooped reference, using deliberately small and uneven
// buffers so every Read crosses internal packet and page boundaries.
func TestInfiniteLoopWaveform(t *testing.T) {
	const start, end = 10000, 40000
	r := openLoopReader(t, "testdata/loop.ogg", true)
	ref := decodeReference(t, "testdata/loop.ogg")
	ch := r.Channels()
	if err := r.SetLoop(start, end); err != nil {
		t.Fatal(err)
	}

	const frames = 100000 // > three full loop regions
	buf := make([]float32, 137*ch)
	out := make([]float32, 0, frames*ch)
	for len(out)/ch < frames {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("reader made no progress")
		}
	}
	out = out[:frames*ch]
	if got := len(out) / ch; got != frames {
		t.Fatalf("decoded %d frames, want %d", got, frames)
	}
	for i := 0; i < frames; i++ {
		src := infiniteSource(int64(i), start, end-start)
		if !framesEqual(frameAt(out, ch, i), frameAt(ref, ch, int(src)), 1e-5) {
			t.Fatalf("frame %d differs from reference frame %d", i, src)
		}
	}
}

// TestLoopBoundaryInOneRead forces a single Read to span loopEnd so the
// returned buffer contains the region's tail immediately followed by the
// loop start: continuous PCM across the boundary within one call.
func TestLoopBoundaryInOneRead(t *testing.T) {
	const start, end = 10000, 40000
	r := openLoopReader(t, "testdata/loop.ogg", true)
	ref := decodeReference(t, "testdata/loop.ogg")
	ch := r.Channels()
	if err := r.SetLoop(start, end); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPosition(end - 50); err != nil {
		t.Fatal(err)
	}
	// Large enough to cover tail (50 frames), the wrap and more frames.
	buf := make([]float32, 300*ch)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := n / ch
	if got < 100 {
		t.Fatalf("only got %d frames across the boundary", got)
	}
	for i := 0; i < got; i++ {
		var src int64
		if i < 50 {
			src = end - 50 + int64(i)
		} else {
			src = start + int64(i-50)
		}
		if !framesEqual(frameAt(buf[:n], ch, i), frameAt(ref, ch, int(src)), 1e-5) {
			t.Fatalf("boundary frame %d (source %d) differs", i, src)
		}
	}
	if pos := r.Position(); pos != start+int64(got-50) {
		t.Fatalf("position after boundary read = %d, want %d", pos, start+int64(got-50))
	}
}

// TestFiniteLoop counts repetitions and checks the post-loop tail resumes
// linear playback at loopEnd and reaches the natural end of the stream.
func TestFiniteLoop(t *testing.T) {
	const start, end, count = 10000, 40000, 3
	r := openLoopReader(t, "testdata/loop.ogg", true)
	ref := decodeReference(t, "testdata/loop.ogg")
	ch := r.Channels()
	length := int(r.Length())
	if err := r.SetLoopCount(start, end, count); err != nil {
		t.Fatal(err)
	}
	if r.LoopRemaining() != count {
		t.Fatalf("LoopRemaining = %d, want %d", r.LoopRemaining(), count)
	}

	buf := make([]float32, 277*ch) // odd, smaller than a decoded packet
	var out []float32
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	wantFrames := start + count*(end-start) + (int64(length) - end)
	if got := len(out) / ch; int64(got) != wantFrames {
		t.Fatalf("decoded %d frames, want %d", got, wantFrames)
	}
	if r.LoopEnabled() || r.LoopRemaining() != 0 {
		t.Fatal("looping should be disabled once the count is exhausted")
	}
	for i := 0; i < len(out)/ch; i++ {
		var src int64
		switch {
		case i < start:
			src = int64(i)
		case int64(i) < start+count*(end-start):
			src = start + (int64(i)-start)%(end-start)
		default:
			src = end + (int64(i) - (start + count*(end-start)))
		}
		if !framesEqual(frameAt(out, ch, i), frameAt(ref, ch, int(src)), 1e-5) {
			t.Fatalf("frame %d differs from reference frame %d", i, src)
		}
	}
}

func TestLoopPositionConsistency(t *testing.T) {
	const start, end = 8000, 16000
	r := openLoopReader(t, "testdata/loop_mono.ogg", true)
	if err := r.SetLoop(start, end); err != nil {
		t.Fatal(err)
	}
	// Position inside the region is accepted and reported back exactly.
	if err := r.SetPosition(end - 10); err != nil {
		t.Fatal(err)
	}
	if pos := r.Position(); pos != end-10 {
		t.Fatalf("Position = %d, want %d", pos, end-10)
	}
	// Positions outside the region are rejected without moving the reader.
	for _, bad := range []int64{0, start - 1, end, end + 100, r.Length()} {
		if err := r.SetPosition(bad); !errors.Is(err, oggvorbis.ErrLoopPositionOutOfRange) {
			t.Fatalf("SetPosition(%d) error = %v, want ErrLoopPositionOutOfRange", bad, err)
		}
		if pos := r.Position(); pos != end-10 {
			t.Fatalf("rejected seek moved position to %d", pos)
		}
	}
	// Read across the end: Position wraps into the region and stays inside.
	buf := make([]float32, 200)
	total := 0
	for total < 500 {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		total += n
		if pos := r.Position(); pos < start || pos > end {
			t.Fatalf("Position %d left the loop region [%d,%d)", pos, start, end)
		}
	}
}

func TestDisableLoop(t *testing.T) {
	const start, end = 10000, 40000
	r := openLoopReader(t, "testdata/loop.ogg", true)
	ref := decodeReference(t, "testdata/loop.ogg")
	ch := r.Channels()
	if err := r.SetLoop(start, end); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPosition(39000); err != nil {
		t.Fatal(err)
	}
	r.DisableLoop()
	if r.LoopEnabled() {
		t.Fatal("looping should be disabled")
	}
	// After disabling, playback runs linearly past the former loopEnd all
	// the way to the natural EOF.
	buf := make([]float32, 512*ch)
	var out []float32
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	wantFrames := int(r.Length()) - 39000
	if got := len(out) / ch; got != wantFrames {
		t.Fatalf("decoded %d frames after disable, want %d", got, wantFrames)
	}
	for i := 0; i < len(out)/ch; i++ {
		src := 39000 + i
		if !framesEqual(frameAt(out, ch, i), frameAt(ref, ch, src), 1e-5) {
			t.Fatalf("frame %d differs from reference frame %d", i, src)
		}
	}
}

func TestSetLoopValidation(t *testing.T) {
	r := openLoopReader(t, "testdata/loop.ogg", true)
	length := r.Length()
	tests := []struct {
		name       string
		start, end int64
		want       error
	}{
		{"negative start", -1, 100, oggvorbis.ErrLoopPointsInvalid},
		{"empty region", 100, 100, oggvorbis.ErrLoopPointsInvalid},
		{"inverted", 200, 100, oggvorbis.ErrLoopPointsInvalid},
		{"end beyond length", 100, length + 1, oggvorbis.ErrLoopPointsOutOfRange},
		{"start beyond length", length, length + 10, oggvorbis.ErrLoopPointsOutOfRange},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.SetLoop(tc.start, tc.end); !errors.Is(err, tc.want) {
				t.Fatalf("SetLoop error = %v, want %v", err, tc.want)
			}
		})
	}
	if err := r.SetLoopCount(100, 200, 0); !errors.Is(err, oggvorbis.ErrLoopPointsInvalid) {
		t.Fatalf("SetLoopCount(0) error = %v", err)
	}
	// End exactly at stream length is legal.
	if err := r.SetLoop(0, length); err != nil {
		t.Fatalf("SetLoop(0, length): %v", err)
	}
}

// TestMonoLoop exercises the single-channel fixture across its boundary.
func TestMonoLoop(t *testing.T) {
	const start, end = 8000, 16000
	r := openLoopReader(t, "testdata/loop_mono.ogg", true)
	ref := decodeReference(t, "testdata/loop_mono.ogg")
	if r.Channels() != 1 {
		t.Fatalf("channels = %d, want 1", r.Channels())
	}
	if err := r.SetLoop(start, end); err != nil {
		t.Fatal(err)
	}
	// One-byte-at-a-time style small reads to stress boundary handling.
	buf := make([]float32, 31)
	got := make([]float32, 0, 20000)
	for len(got) < 20000 {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("no progress")
		}
	}
	for i := 0; i < len(got); i++ {
		src := infiniteSource(int64(i), start, end-start)
		d := got[i] - ref[src]
		if d < 0 {
			d = -d
		}
		if d > 1e-5 {
			t.Fatalf("frame %d differs from reference frame %d", i, src)
		}
	}
}

// TestWrapCounting verifies a finite loop plays the region exactly the
// requested number of times and that LoopRemaining tracks the repetitions.
func TestWrapCounting(t *testing.T) {
	const start, end, count = 10000, 40000, 4
	r := openLoopReader(t, "testdata/loop.ogg", true)
	if err := r.SetLoopCount(start, end, count); err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 64*r.Channels())
	emitted := 0
	regions := 0
	prevRemaining := r.LoopRemaining()
	for {
		n, err := r.Read(buf)
		emitted += n / r.Channels()
		if rem := r.LoopRemaining(); rem < prevRemaining {
			// One full region was completed.
			regions++
			prevRemaining = rem
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if regions != count {
		t.Fatalf("completed %d regions, want %d", regions, count)
	}
	if r.LoopRemaining() != 0 {
		t.Fatalf("LoopRemaining = %d, want 0", r.LoopRemaining())
	}
	wantFrames := start + count*(end-start) + (r.Length() - end)
	if int64(emitted) != wantFrames {
		t.Fatalf("total frames %d, want %d", emitted, wantFrames)
	}
}

// TestExactBoundaryRead reads a region whose end coincides with an exact
// number of small reads, checking Position reports loopEnd before the wrap.
func TestExactBoundaryRead(t *testing.T) {
	const start, end = 10000, 40000
	r := openLoopReader(t, "testdata/loop.ogg", true)
	ref := decodeReference(t, "testdata/loop.ogg")
	ch := r.Channels()
	if err := r.SetLoop(start, end); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPosition(end - 100); err != nil {
		t.Fatal(err)
	}
	// Read exactly the 100 remaining frames of the region in two calls.
	first := make([]float32, 40*ch)
	second := make([]float32, 60*ch)
	if _, err := r.Read(first); err != nil {
		t.Fatal(err)
	}
	if pos := r.Position(); pos != end-60 {
		t.Fatalf("Position = %d, want %d", pos, end-60)
	}
	if _, err := r.Read(second); err != nil {
		t.Fatal(err)
	}
	// The boundary itself is reached; the very next frame must be loopStart.
	if pos := r.Position(); pos != end {
		t.Fatalf("Position at boundary = %d, want %d", pos, end)
	}
	next := make([]float32, 10*ch)
	if _, err := r.Read(next); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if !framesEqual(frameAt(next, ch, i), frameAt(ref, ch, int(start)+i), 1e-5) {
			t.Fatalf("post-wrap frame %d does not match loopStart region", i)
		}
	}
}

// TestInfiniteLoopBounded confirms an infinite loop never spins without
// producing data: every Read returns frames and Position stays in range.
func TestInfiniteLoopBounded(t *testing.T) {
	const start, end = 10000, 40000
	r := openLoopReader(t, "testdata/loop.ogg", true)
	if err := r.SetLoop(start, end); err != nil {
		t.Fatal(err)
	}
	if r.LoopRemaining() != -1 {
		t.Fatalf("infinite LoopRemaining = %d, want -1", r.LoopRemaining())
	}
	buf := make([]float32, 17*r.Channels())
	emitted := 0
	for emitted < 250000 {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("infinite loop made no progress")
		}
		emitted += n / r.Channels()
		// Before first reaching loopStart the reader plays the linear
		// lead-in; once inside, every position stays within the region.
		if pos := r.Position(); emitted > start && (pos < start || pos > end) {
			t.Fatalf("Position %d outside region", pos)
		}
	}
}

// TestLoopSpanningWholeStream uses loopStart=0 and loopEnd=stream length,
// including a finite count where the final jump lands exactly at EOF.
func TestLoopSpanningWholeStream(t *testing.T) {
	r := openLoopReader(t, "testdata/loop.ogg", true)
	ref := decodeReference(t, "testdata/loop.ogg")
	ch := r.Channels()
	length := r.Length()
	if err := r.SetLoopCount(0, length, 2); err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 113*ch)
	var out []float32
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	// Two full plays of the stream and nothing after.
	if int64(len(out)/ch) != 2*length {
		t.Fatalf("frames = %d, want %d", len(out)/ch, 2*length)
	}
	for i := 0; i < len(out)/ch; i++ {
		src := int64(i) % length
		if !framesEqual(frameAt(out, ch, i), frameAt(ref, ch, int(src)), 1e-5) {
			t.Fatalf("frame %d differs from reference frame %d", i, src)
		}
	}
}

// TestDefaultReaderUnchanged makes sure a reader with looping never enabled
// still decodes the whole stream exactly as before, via small buffers.
func TestDefaultReaderUnchanged(t *testing.T) {
	r := openLoopReader(t, "testdata/loop.ogg", true)
	ref := decodeReference(t, "testdata/loop.ogg")
	ch := r.Channels()
	buf := make([]float32, 99*ch)
	var out []float32
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) != len(ref) {
		t.Fatalf("decoded %d values, want %d", len(out), len(ref))
	}
	for i := range out {
		d := out[i] - ref[i]
		if d < 0 {
			d = -d
		}
		if d > 1e-6 {
			t.Fatalf("value %d changed: %g != %g", i, out[i], ref[i])
		}
	}
}
