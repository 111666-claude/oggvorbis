package oggvorbis

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/jfreymuth/vorbis"
)

// A Reader can read audio from an ogg/vorbis file.
//
// Files consisting of several chained vorbis streams are supported: when a
// stream ends (EOS) and a new stream begins (BOS with a new serial number),
// the reader transparently re-reads the vorbis headers and continues
// decoding, provided the new stream has the same sample rate and number of
// channels. Positions and lengths are reported in cumulative samples across
// all chained streams.
type Reader struct {
	r   oggReader
	dec vorbis.Decoder

	sampleRate int
	channels   int

	position       int64 // position within the current segment, in samples
	segBase        int64 // cumulative offset of the current segment, in samples
	buffer         []float32
	originalBuffer []float32
	toSkip         int
	checkOffset    bool

	length   int64
	segments []segment
}

// NewReader creates a new Reader.
// Some of the returned reader's methods will only work if in also implements
// io.Seeker
func NewReader(in io.Reader) (*Reader, error) {
	r := new(Reader)
	r.r.source = in
	r.r.seeker, _ = in.(io.Seeker)
	if err := r.init(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reader) init() error {
	if r.r.seeker != nil {
		segments, err := r.r.scanChain()
		if err != nil {
			return err
		}
		r.segments = segments
		last := segments[len(segments)-1]
		r.length = last.base + last.length
		if _, err := r.r.seeker.Seek(0, io.SeekStart); err != nil {
			return err
		}
		r.r.buffer = nil
	}

	if err := r.readHeaders(); err != nil {
		return err
	}
	r.sampleRate = r.dec.SampleRate()
	r.channels = r.dec.Channels()

	return r.prime()
}

// readHeaders reads the three vorbis header packets of the current segment
// into a fresh decoder.
func (r *Reader) readHeaders() error {
	r.dec = vorbis.Decoder{}
	for i := 0; i < 3; i++ {
		packet, err := r.r.NextPacket()
		if err != nil {
			return noEOF(err)
		}
		if err := r.dec.ReadHeader(packet); err != nil {
			return err
		}
	}
	return nil
}

// checkFormat verifies that the current segment has the same format as the
// first segment of the file.
func (r *Reader) checkFormat() error {
	if r.dec.SampleRate() != r.sampleRate || r.dec.Channels() != r.channels {
		return fmt.Errorf("oggvorbis: chained stream has different format (%d Hz, %d channels; expected %d Hz, %d channels)",
			r.dec.SampleRate(), r.dec.Channels(), r.sampleRate, r.channels)
	}
	return nil
}

// prime reads the first packets of the current segment, because the data
// might not start at position zero, see:
// https://xiph.org/vorbis/doc/Vorbis_I_spec.html#x1-132000A.2
func (r *Reader) prime() error {
	if size := r.dec.BufferSize(); size != len(r.originalBuffer) {
		r.originalBuffer = make([]float32, size)
	}
	r.checkOffset = true
	if err := r.fillBuffer(); err != nil {
		return err
	}
	// The check is a one-shot: if priming ran into further empty segments
	// recursively, the innermost prime already performed it.
	if r.checkOffset {
		r.checkOffset = false
		if r.r.packetIndex == r.r.currentPage.packetCount {
			offset := r.r.currentPage.AbsoluteGranulePosition - r.position
			if offset < 0 {
				r.buffer = r.buffer[-offset:]
			}
			r.position = r.r.currentPage.AbsoluteGranulePosition
		}
	}
	return nil
}

// startSegment begins decoding the next chained segment: the vorbis headers
// are re-read, the decoder state is reset and the format is checked against
// the first segment.
func (r *Reader) startSegment() error {
	r.segBase += r.position
	r.position = 0
	if err := r.readHeaders(); err != nil {
		return err
	}
	if err := r.checkFormat(); err != nil {
		return err
	}
	return r.prime()
}

// SampleRate returns the sample rate of the vorbis stream.
func (r *Reader) SampleRate() int { return r.dec.SampleRate() }

// Channels returns the number of channels of the vorbis stream.
func (r *Reader) Channels() int { return r.dec.Channels() }

// Bitrate returns a struct containing information about the bitrate.
func (r *Reader) Bitrate() vorbis.Bitrate { return r.dec.Bitrate }

// CommentHeader returns a struct containing info from the comment header.
func (r *Reader) CommentHeader() vorbis.CommentHeader { return r.dec.CommentHeader }

// Position returns the current position in samples. For chained files the
// position is cumulative across all segments.
func (r *Reader) Position() int64 {
	return r.segBase + r.position - int64(len(r.buffer)/r.Channels()) + int64(r.toSkip/r.Channels())
}

// Length returns the length of the audio data in samples. For chained files
// the lengths of all segments are added up.
// A return value of zero means the length is unknown, probably because the
// underlying reader is not seekable.
func (r *Reader) Length() int64 { return r.length }

// SetPosition seeks to a position in samples. For chained files the position
// is cumulative across all segments, so any segment and segment boundary can
// be reached.
// It will return an error if the underlying reader is not seekable.
func (r *Reader) SetPosition(pos int64) error {
	if r.r.seeker == nil {
		return errors.New("oggvorbis: reader is not seekable")
	}
	r.buffer = nil
	r.toSkip = 0
	if pos >= r.length {
		r.r.lastPacket = true
		r.segBase = 0
		r.position = r.length
		return nil
	}
	if pos < 0 {
		pos = 0
	}
	// find the segment containing pos
	i := sort.Search(len(r.segments), func(i int) bool {
		return r.segments[i].base+r.segments[i].length > pos
	})
	seg := r.segments[i]
	if _, err := r.r.seeker.Seek(seg.offset, io.SeekStart); err != nil {
		return err
	}
	r.r.reset()
	if err := r.readHeaders(); err != nil {
		return err
	}
	if err := r.checkFormat(); err != nil {
		return err
	}
	if size := r.dec.BufferSize(); size != len(r.originalBuffer) {
		r.originalBuffer = make([]float32, size)
	}
	r.segBase = seg.base
	r.position = 0
	local := pos - seg.base
	start, err := r.r.SeekPageBefore(local)
	if err != nil {
		return err
	}
	packet, err := r.r.NextPacket()
	if err != nil {
		return err
	}
	r.dec.Clear()
	r.dec.DecodeInto(packet, r.originalBuffer)
	r.position = start
	r.toSkip = int(local-start) * r.Channels()
	return nil
}

// Read reads and decodes audio data and stores the result in p.
//
// It returns the number of values decoded (number of samples * channels) and
// any error encountered, similarly to an io.Reader's Read method.
//
// The number of values produced will always be a multiple of Channels().
func (r *Reader) Read(p []float32) (int, error) {
	if r.r.lastPacket && len(r.buffer) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(p)%r.Channels() != 0 {
		p = p[:len(p)/r.Channels()*r.Channels()]
	}
	out := p
	total := 0
	err := error(nil)
	if r.toSkip > 0 {
		err = r.skip()
		if err != nil {
			return 0, err
		}
	}
	if len(r.buffer) > 0 {
		n := copy(p, r.buffer)
		r.buffer = r.buffer[n:]
		total += n
		p = p[n:]
	}
	for len(p) >= len(r.originalBuffer) {
		var n int
		n, err = r.read(p)
		total += n
		if err != nil {
			goto end
		}
		p = p[n:]
	}
	if total == 0 {
		err = r.fillBuffer()
		if err != nil {
			goto end
		}
		n := copy(p, r.buffer)
		r.buffer = r.buffer[n:]
		total += n
		p = p[n:]
	}
end:
	for i := range out[:total] {
		if out[i] > 1 {
			out[i] = 1
		} else if out[i] < -1 {
			out[i] = -1
		}
	}
	return total, err
}

func (r *Reader) fillBuffer() error {
	n, err := r.read(r.originalBuffer)
	r.buffer = r.originalBuffer[:n]
	if err != nil {
		return err
	}
	if n == 0 {
		return r.fillBuffer()
	}
	return nil
}

func (r *Reader) read(p []float32) (int, error) {
	for {
		packet, err := r.r.NextPacket()
		if err == errSegmentStart {
			// The previous segment ended and a new chained stream
			// begins: re-read its headers and continue seamlessly.
			if err := r.startSegment(); err != nil {
				return 0, err
			}
			if len(r.buffer) > 0 {
				// priming produced samples of the new segment; flush
				// them first to keep the output order intact
				n := copy(p, r.buffer)
				r.buffer = r.buffer[n:]
				return n, nil
			}
			continue
		}
		if err != nil {
			if err == io.EOF && r.length == 0 {
				r.length = r.segBase + r.position
			}
			return 0, err
		}
		out, err := r.dec.DecodeInto(packet, p)
		n := len(out)
		r.position += int64(n / r.Channels())
		if err != nil {
			return n, err
		}
		if r.r.segmentEnd {
			r.r.segmentEnd = false
			discard := int(r.position-r.r.currentPage.AbsoluteGranulePosition) * r.Channels()
			if discard > 0 {
				n -= discard
			}
			r.position = r.r.currentPage.AbsoluteGranulePosition
		}
		return n, nil
	}
}

func (r *Reader) skip() error {
	for {
		err := r.fillBuffer()
		if len(r.buffer) > r.toSkip {
			r.buffer = r.buffer[r.toSkip:]
			r.toSkip = 0
			return err
		}
		r.toSkip -= len(r.buffer)
		if err != nil {
			return err
		}
	}
}
