package oggvorbis

import (
	"errors"
	"io"
	"sort"
)

// DefaultSeekIndexInterval is the default distance (in samples per channel)
// between two entries of the sparse seek index built for seekable inputs.
const DefaultSeekIndexInterval = 1 << 16

// Errors used internally to signal that no seek index can be built; they
// never reach the caller directly: building the index is best-effort and any
// failure simply disables the index, falling back to a linear page scan.
var (
	errChainedStream = errors.New("ogg: chained or multiplexed streams are not indexable")
	errBadGranule    = errors.New("ogg: invalid or non-monotonic granule position")
)

// seekIndexEntry maps a sample position to the file offset of a safe anchor
// page: a page that does not start with a continued packet and carries a
// valid granule position, so decoding can restart there at a packet boundary.
type seekIndexEntry struct {
	granule int64
	offset  int64
}

// seekIndex is a sparse, append-only index of safe anchor pages. Memory use
// is bounded by length/interval+1 entries (16 bytes each), independent of how
// often SetPosition is called.
type seekIndex struct {
	interval int64
	entries  []seekIndexEntry
}

// lookup returns the index entry with the greatest granule position <= pos.
func (idx *seekIndex) lookup(pos int64) (offset, granule int64, ok bool) {
	i := sort.Search(len(idx.entries), func(i int) bool {
		return idx.entries[i].granule > pos
	}) - 1
	if i < 0 {
		return 0, 0, false
	}
	return idx.entries[i].offset, idx.entries[i].granule, true
}

// buildSeekIndex scans the page headers of the stream and records safe anchor
// pages spaced at least interval samples apart. It only reads page headers
// and seeks over page content, so it is O(pages) in time and O(entries) in
// memory. The stream position is left undefined; callers must re-seek.
//
// The build fails (and the caller disables the index) when the stream is
// chained/multiplexed or carries invalid granule positions; decoding and
// seeking then keep working via the linear scan.
func buildSeekIndex(in io.Reader, seeker io.Seeker, interval int64) (*seekIndex, error) {
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	r := &oggReader{source: in, seeker: seeker}
	idx := &seekIndex{interval: interval}
	var p page
	var serial uint32
	haveSerial := false
	haveGranule := false
	var prevGranule, lastIndexed int64
	for {
		offset, _ := seeker.Seek(0, io.SeekCurrent)
		err := p.readHeader(r)
		if err == io.EOF {
			break // clean end of stream without an EOS page
		}
		if err != nil {
			return nil, err
		}
		if !haveSerial {
			serial = p.StreamSerialNumber
			haveSerial = true
		} else if p.StreamSerialNumber != serial || p.isFirst() {
			// A second BOS page or a serial change means a chained or
			// multiplexed stream; granule positions restart per chain and
			// a single index cannot describe the stream.
			return nil, errChainedStream
		}
		g := p.AbsoluteGranulePosition
		if g < -1 {
			return nil, errBadGranule
		}
		if g >= 0 {
			if haveGranule && g < prevGranule {
				return nil, errBadGranule
			}
			haveGranule = true
			prevGranule = g
			safe := p.HeaderTypeFlag&headerFlagContinuedPacket == 0
			if safe && (len(idx.entries) == 0 || g-lastIndexed >= interval) {
				idx.entries = append(idx.entries, seekIndexEntry{granule: g, offset: offset})
				lastIndexed = g
			}
		}
		if p.isLast() {
			// A single stream ends here. Anything following the EOS page
			// (a second chain, trailing garbage) cannot be described by
			// one index, so give up instead of building a wrong one.
			seeker.Seek(int64(p.totalSize-len(r.buffer)), io.SeekCurrent)
			r.buffer = nil
			var next page
			if err := next.readHeader(r); err != io.EOF {
				return nil, errChainedStream
			}
			break
		}
		seeker.Seek(int64(p.totalSize-len(r.buffer)), io.SeekCurrent)
		r.buffer = nil
	}
	if len(idx.entries) == 0 {
		return nil, errBadGranule
	}
	return idx, nil
}
