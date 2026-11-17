package oggvorbis

import (
	"errors"
	"strconv"
	"strings"
)

// Errors returned by the loop-point API. Callers can use errors.Is to check
// for a specific condition.
var (
	// ErrLoopPointsMissing is returned when the Vorbis comment header does
	// not contain both a LOOPSTART and a LOOPEND tag.
	ErrLoopPointsMissing = errors.New("oggvorbis: missing LOOPSTART/LOOPEND comments")
	// ErrLoopPointsDuplicate is returned when LOOPSTART or LOOPEND appears
	// more than once in the Vorbis comment header.
	ErrLoopPointsDuplicate = errors.New("oggvorbis: duplicate LOOPSTART/LOOPEND comments")
	// ErrLoopPointsInvalid is returned when a loop tag is not a non-negative
	// integer, or when LOOPSTART is not smaller than LOOPEND.
	ErrLoopPointsInvalid = errors.New("oggvorbis: invalid loop points")
	// ErrLoopPointsOutOfRange is returned when a loop point lies outside the
	// decoded audio. This can only be detected when the reader is seekable
	// and therefore knows the stream's length.
	ErrLoopPointsOutOfRange = errors.New("oggvorbis: loop points are outside the audio")
	// ErrLoopPositionOutOfRange is returned by SetPosition when looping is
	// enabled and the requested position is not inside the loop region.
	ErrLoopPositionOutOfRange = errors.New("oggvorbis: position is outside the loop region")
)

// LoopPoints reads the LOOPSTART and LOOPEND sample coordinates from the
// stream's Vorbis comment header.
//
// The tags are matched case-insensitively, as required by the Vorbis
// specification. Playback repeats the samples in the half-open interval
// [start, end): the sample at end is the first sample not part of the loop
// region.
//
// It returns an error when a tag is missing, duplicated, not a non-negative
// integer, when start >= end, or (for seekable streams) when the points lie
// beyond the end of the audio. A non-seekable reader cannot know the stream
// length, so the bounds check is skipped in that case.
func (r *Reader) LoopPoints() (start, end int64, err error) {
	var gotStart, gotEnd bool
	for _, c := range r.dec.CommentHeader.Comments {
		eq := strings.IndexByte(c, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToUpper(c[:eq])
		value := c[eq+1:]
		switch key {
		case "LOOPSTART":
			if gotStart {
				return 0, 0, ErrLoopPointsDuplicate
			}
			start, err = parseLoopPoint(value)
			if err != nil {
				return 0, 0, err
			}
			gotStart = true
		case "LOOPEND":
			if gotEnd {
				return 0, 0, ErrLoopPointsDuplicate
			}
			end, err = parseLoopPoint(value)
			if err != nil {
				return 0, 0, err
			}
			gotEnd = true
		}
	}
	if !gotStart || !gotEnd {
		return 0, 0, ErrLoopPointsMissing
	}
	if start >= end {
		return 0, 0, ErrLoopPointsInvalid
	}
	if r.length > 0 && (start >= r.length || end > r.length) {
		return 0, 0, ErrLoopPointsOutOfRange
	}
	return start, end, nil
}

func parseLoopPoint(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, ErrLoopPointsInvalid
	}
	return n, nil
}

// SetLoop enables infinite looping over the half-open region [start, end).
// Playback jumps back to start every time end is reached.
//
// Looping requires a seekable underlying reader. The coordinates must
// satisfy 0 <= start < end, and end must not exceed the stream length.
func (r *Reader) SetLoop(start, end int64) error {
	return r.enableLoop(start, end, -1)
}

// SetLoopCount enables finite looping over the half-open region [start, end).
// The region is played count times in total (count >= 1); after the last
// repetition playback continues past end until the end of the stream.
//
// Like SetLoop, it requires a seekable reader and valid coordinates.
func (r *Reader) SetLoopCount(start, end int64, count int) error {
	if count < 1 {
		return ErrLoopPointsInvalid
	}
	return r.enableLoop(start, end, int64(count))
}

// SetLoopFromTags enables infinite looping using the loop points stored in
// the Vorbis comment header. It is equivalent to calling LoopPoints followed
// by SetLoop.
func (r *Reader) SetLoopFromTags() error {
	start, end, err := r.LoopPoints()
	if err != nil {
		return err
	}
	return r.SetLoop(start, end)
}

// SetLoopCountFromTags enables finite looping using the loop points stored
// in the Vorbis comment header. It is equivalent to calling LoopPoints
// followed by SetLoopCount.
func (r *Reader) SetLoopCountFromTags(count int) error {
	start, end, err := r.LoopPoints()
	if err != nil {
		return err
	}
	return r.SetLoopCount(start, end, count)
}

func (r *Reader) enableLoop(start, end, count int64) error {
	if r.r.seeker == nil {
		return errors.New("oggvorbis: reader is not seekable")
	}
	if start < 0 || start >= end {
		return ErrLoopPointsInvalid
	}
	if r.length > 0 && end > r.length {
		return ErrLoopPointsOutOfRange
	}
	r.looping = true
	r.loopStart = start
	r.loopEnd = end
	r.loopCount = count
	r.loopAtEnd = false
	r.loopSeekPos = 0
	return nil
}

// DisableLoop turns off looping and restores ordinary, linear playback.
// The current position is not changed.
func (r *Reader) DisableLoop() {
	r.looping = false
	r.loopStart = 0
	r.loopEnd = 0
	r.loopCount = 0
	r.loopAtEnd = false
	r.loopSeekPos = 0
}

// LoopEnabled reports whether looping is currently active.
func (r *Reader) LoopEnabled() bool { return r.looping }

// LoopRemaining returns the number of times the loop region will still be
// played, including the current one. It returns -1 for infinite looping and
// zero when looping is disabled.
func (r *Reader) LoopRemaining() int64 {
	if !r.looping {
		return 0
	}
	return r.loopCount
}

// readLooped is the loop-aware implementation of Read.
//
// A decoded packet is never truncated: the Vorbis decoder produces each
// packet's samples with overlap-add against the previous packet, so cutting a
// packet short would corrupt the waveform. Instead a packet that crosses the
// loop end is decoded fully, delivered only up to loopEnd, and then the
// reader seeks to loopStart; the seek's preroll samples are discarded via
// toSkip, which is the same mechanism SetPosition uses. A single Read thus
// returns continuous PCM across the boundary.
func (r *Reader) readLooped(p []float32) (int, error) {
	out := p
	total := 0
	for total < len(p) {
		// A previous fill reached loopEnd. Finish delivering the buffered
		// samples up to the boundary, then jump.
		if r.loopAtEnd {
			if len(r.buffer) > 0 {
				n := copy(p[total:], r.buffer)
				r.buffer = r.buffer[n:]
				total += n
				continue
			}
			r.loopAtEnd = false
			target := r.loopSeekPos
			if err := r.seekTo(target); err != nil {
				r.clip(out[:total])
				return total, err
			}
			continue
		}
		if r.toSkip > 0 {
			if len(r.buffer) == 0 {
				if err := r.fillBufferLooped(); err != nil {
					r.clip(out[:total])
					return total, err
				}
			}
			n := r.toSkip
			if n > len(r.buffer) {
				n = len(r.buffer)
			}
			r.buffer = r.buffer[n:]
			r.toSkip -= n
			continue
		}
		if len(r.buffer) == 0 {
			if err := r.fillBufferLooped(); err != nil {
				r.clip(out[:total])
				return total, err
			}
			continue
		}
		n := copy(p[total:], r.buffer)
		r.buffer = r.buffer[n:]
		total += n
	}
	r.clip(out[:total])
	return total, nil
}

func (r *Reader) clip(s []float32) {
	for i := range s {
		if s[i] > 1 {
			s[i] = 1
		} else if s[i] < -1 {
			s[i] = -1
		}
	}
}

// readLoop decodes a single packet into the internal buffer via
// fillBufferLooped and updates the loop state when the packet reaches the
// loop end.
//
// When the decoded packet would run past loopEnd, only the samples up to the
// boundary are left in r.buffer; the rest are discarded by shortening the
// slice (the packet itself was decoded in full, so the decoder's overlap-add
// state stays valid until the seek resets it).
func (r *Reader) readLoop() error {
	packet, err := r.r.NextPacket()
	if err != nil {
		return err
	}
	out, derr := r.dec.DecodeInto(packet, r.originalBuffer)
	n := len(out)
	ch := r.Channels()
	packetStart := r.position
	r.position += int64(n / ch)
	if derr != nil {
		r.buffer = r.originalBuffer[:n]
		return derr
	}
	if r.r.lastPacket {
		discard := int(r.position-r.r.currentPage.AbsoluteGranulePosition) * ch
		if discard > 0 {
			n -= discard
		}
		r.position = r.r.currentPage.AbsoluteGranulePosition
		r.length = r.position
	}
	r.buffer = r.originalBuffer[:n]
	if r.looping && r.position >= r.loopEnd {
		// Deliver only the part of the packet within the region.
		keep := int(r.loopEnd-packetStart) * ch
		if keep > n {
			keep = n
		}
		if keep < 0 {
			keep = 0
		}
		r.buffer = r.originalBuffer[:keep]
		r.position = r.loopEnd
		r.loopAtEnd = true
		if r.loopCount > 0 {
			r.loopCount--
			if r.loopCount == 0 {
				// Finite loop exhausted: continue linearly from loopEnd.
				r.looping = false
				r.loopSeekPos = r.loopEnd
				return nil
			}
		}
		r.loopSeekPos = r.loopStart
	}
	return nil
}

func (r *Reader) fillBufferLooped() error {
	if err := r.readLoop(); err != nil {
		return err
	}
	if len(r.buffer) == 0 && !r.loopAtEnd {
		// The packet produced no samples (legal vorbis); decode another one
		// instead of returning without progress.
		return r.fillBufferLooped()
	}
	return nil
}

// seekTo positions the underlying ogg stream at the page containing pos and
// prepares the decoder so the next decoded packet covers pos. The reader's
// buffer is cleared; samples before pos are accounted for via toSkip.
func (r *Reader) seekTo(pos int64) error {
	if pos >= r.length {
		// Seeking to (or past) the end: mark the stream exhausted instead of
		// asking SeekPageBefore for a page beyond the last one.
		r.buffer = nil
		r.toSkip = 0
		r.r.lastPacket = true
		r.position = r.length
		return nil
	}
	start, err := r.r.SeekPageBefore(pos)
	if err != nil {
		return err
	}
	packet, err := r.r.NextPacket()
	if err != nil {
		return err
	}
	r.dec.Clear()
	r.dec.DecodeInto(packet, r.originalBuffer)
	r.buffer = nil
	r.position = start
	r.toSkip = int(pos-start) * r.Channels()
	return nil
}
