package oggvorbis

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

// errSegmentStart is returned by NextPacket when a new chained logical stream
// begins (a BOS page with a new serial following the previous stream's EOS
// page). The packets of the new stream's first page are returned by the
// subsequent NextPacket calls.
var errSegmentStart = errors.New("ogg: start of new chained stream")

type oggReader struct {
	source io.Reader
	seeker io.Seeker
	buffer []byte

	currentPage      page
	lastPagePosition int64
	packetIndex      int
	ready            bool
	lastPacket       bool

	// chained-stream state
	serial       uint32
	hasSerial    bool
	eos          bool // the current logical stream's EOS page has been read
	segmentStart bool // the page just read starts a new chained stream
	segmentEnd   bool // the packet last returned ends its logical stream
}

func (r *oggReader) NextPacket() ([]byte, error) {
	if !r.ready {
		if err := r.readPage(); err != nil {
			return nil, err
		}
		r.packetIndex = 0
		if r.currentPage.HeaderTypeFlag&headerFlagContinuedPacket != 0 {
			r.packetIndex = 1
		}
		r.ready = true
		if r.segmentStart {
			r.segmentStart = false
			return nil, errSegmentStart
		}
	}
	if r.packetIndex == r.currentPage.packetCount {
		rest := r.currentPage.packets[r.currentPage.packetCount]
		if r.currentPage.AbsoluteGranulePosition != -1 {
			r.lastPagePosition = r.currentPage.AbsoluteGranulePosition
		}
		if err := r.readPage(); err != nil {
			return nil, err
		}
		if r.segmentStart {
			r.segmentStart = false
			if len(rest) > 0 {
				return nil, errors.New("ogg: packet continued across chained streams")
			}
			r.packetIndex = 0
			return nil, errSegmentStart
		}
		if len(rest) > 0 {
			r.currentPage.packets[0] = append(rest, r.currentPage.packets[0]...)
		}
		r.packetIndex = 0
		return r.NextPacket()
	}
	packet := r.currentPage.packets[r.packetIndex]
	r.packetIndex++
	if r.packetIndex == r.currentPage.packetCount && r.currentPage.isLast() {
		r.segmentEnd = true
	}
	return packet, nil
}

// readPage reads the next page and validates its serial number against the
// current logical stream. A page with a different serial is only accepted as
// the BOS page of a chained stream following an EOS page; anything else is
// rejected as multiplexed or corrupt input.
func (r *oggReader) readPage() error {
	err := r.currentPage.read(r)
	if err != nil {
		if err == io.EOF {
			r.lastPacket = true
		}
		return err
	}
	h := &r.currentPage.pageHeader
	switch {
	case !r.hasSerial:
		r.serial = h.StreamSerialNumber
		r.hasSerial = true
	case h.StreamSerialNumber != r.serial:
		if !r.eos || !h.isFirst() {
			return errors.New("ogg: unexpected stream serial number (multiplexed streams are not supported)")
		}
		r.serial = h.StreamSerialNumber
		r.eos = false
		r.segmentStart = true
	case r.eos:
		return errors.New("ogg: unexpected page after end of stream")
	}
	if h.isLast() {
		r.eos = true
	}
	return nil
}

// reset clears all paging and chaining state; the next page read is treated
// as the start of a new file. The caller must have positioned the seeker.
func (r *oggReader) reset() {
	r.buffer = nil
	r.ready = false
	r.lastPacket = false
	r.hasSerial = false
	r.eos = false
	r.segmentStart = false
	r.segmentEnd = false
}

// A segment describes one logical stream of a chained ogg file.
type segment struct {
	offset int64 // byte offset of the segment's first page
	base   int64 // cumulative position of the segment start, in samples
	length int64 // length of the segment in samples
}

// scanChain scans the whole file, validates the chaining structure and
// returns all logical streams with their cumulative sample offsets.
// The seeker is left at the end of the file.
func (r *oggReader) scanChain() ([]segment, error) {
	if _, err := r.seeker.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	r.buffer = nil
	var segments []segment
	var total, lastGranule int64
	var serial uint32
	var hasSerial, eos bool
	for {
		offset, _ := r.seeker.Seek(0, io.SeekCurrent)
		var p page
		err := p.readHeader(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case !hasSerial:
			serial = p.StreamSerialNumber
			hasSerial = true
			segments = append(segments, segment{offset: offset})
		case p.StreamSerialNumber != serial:
			if !eos || !p.isFirst() {
				return nil, errors.New("ogg: unexpected stream serial number (multiplexed streams are not supported)")
			}
			serial = p.StreamSerialNumber
			eos = false
			segments = append(segments, segment{offset: offset, base: total})
		case eos:
			return nil, errors.New("ogg: unexpected page after end of stream")
		}
		if p.AbsoluteGranulePosition != -1 {
			lastGranule = p.AbsoluteGranulePosition
		}
		if p.isLast() {
			eos = true
			total += p.AbsoluteGranulePosition
			segments[len(segments)-1].length = p.AbsoluteGranulePosition
		}
		if _, err := r.seeker.Seek(int64(p.totalSize-len(r.buffer)), io.SeekCurrent); err != nil {
			return nil, err
		}
		r.buffer = nil
	}
	if len(segments) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	if !eos {
		// The last logical stream has no EOS page (truncated file); use the
		// last seen granule position as its length.
		segments[len(segments)-1].length = lastGranule
	}
	return segments, nil
}

// SeekPageBefore seeks to the last page with an AbsoluteGranulePosition <=
// pos and returns that position. The seeker must be positioned at the start
// of a page within the logical stream to search (after its header pages);
// the scan stops at the stream's EOS page. Positions are relative to the
// start of the logical stream.
func (r *oggReader) SeekPageBefore(pos int64) (int64, error) {
	r.buffer = nil
	r.lastPacket = false
	startOffset, _ := r.seeker.Seek(0, io.SeekCurrent)
	var p page
	lastOffset := int64(-1)
	var lastPos int64
	for {
		offset, _ := r.seeker.Seek(0, io.SeekCurrent)
		err := p.readHeader(r)
		if err != nil {
			return 0, err
		}
		if p.AbsoluteGranulePosition > pos {
			if lastOffset < 0 {
				// pos lies before the first page's granule position;
				// start decoding from the beginning of the stream.
				r.seeker.Seek(startOffset, io.SeekStart)
				r.ready = false
				return 0, nil
			}
			r.seeker.Seek(lastOffset, io.SeekStart)
			err := r.currentPage.read(r)
			if err != nil {
				return 0, err
			}
			if lastPos > 0 {
				r.packetIndex = r.currentPage.packetCount - 1
				r.ready = true
			} else {
				r.ready = false
			}
			return lastPos, nil
		}
		if p.isLast() {
			return 0, io.ErrUnexpectedEOF
		}
		lastOffset = offset
		lastPos = p.AbsoluteGranulePosition
		r.seeker.Seek(int64(p.totalSize-len(r.buffer)), io.SeekCurrent)
		r.buffer = nil
	}
}

func (r *oggReader) Read(p []byte) (int, error) {
	if len(r.buffer) > 0 {
		n := copy(p, r.buffer)
		r.buffer = r.buffer[n:]
		return n, nil
	}
	return r.source.Read(p)
}

const (
	headerFlagContinuedPacket   = 1
	headerFlagBeginningOfStream = 2
	headerFlagEndOfStream       = 4
)

var capturePattern = [4]byte{'O', 'g', 'g', 'S'}

const maxPageSize = 27 + 255 + 255*255

type pageHeader struct {
	CapturePattern          [4]byte
	StreamStructureVersion  uint8
	HeaderTypeFlag          byte
	AbsoluteGranulePosition int64
	StreamSerialNumber      uint32
	PageSequenceNumber      uint32
	PageChecksum            uint32
	PageSegments            uint8
}

func (p *pageHeader) isFirst() bool { return p.HeaderTypeFlag&headerFlagBeginningOfStream != 0 }
func (p *pageHeader) isLast() bool  { return p.HeaderTypeFlag&headerFlagEndOfStream != 0 }

type page struct {
	pageHeader
	headerChecksum uint32
	packetCount    int
	packetSizes    []int
	totalSize      int
	needsContinue  bool
	packets        [][]byte
}

func (p *page) read(r io.Reader) error {
	err := p.readHeader(r)
	if err != nil {
		return err
	}
	return p.readContent(r)
}

func (p *page) readHeader(r io.Reader) error {
	data := make([]byte, 27)
	_, err := io.ReadFull(r, data)
	if err != nil {
		return err
	}
	binary.Read(bytes.NewReader(data), binary.LittleEndian, &p.pageHeader)
	if p.CapturePattern != capturePattern {
		return errors.New("ogg: missing capture pattern")
	}
	if p.StreamStructureVersion != 0 {
		return errors.New("ogg: unsupported version")
	}
	segmentTable := make([]byte, p.PageSegments)
	_, err = io.ReadFull(r, segmentTable)
	if err != nil {
		return noEOF(err)
	}
	data[22], data[23], data[24], data[25] = 0, 0, 0, 0
	p.headerChecksum = crcUpdate(0, data)
	p.headerChecksum = crcUpdate(p.headerChecksum, segmentTable)

	size := 0
	p.totalSize = 0
	p.packetCount = 0
	p.packetSizes = nil
	for _, s := range segmentTable {
		size += int(s)
		p.totalSize += int(s)
		if s < 0xFF {
			p.packetCount++
			p.packetSizes = append(p.packetSizes, size)
			size = 0
		}
	}
	// A page may legitimately have zero segments; guard against the
	// uint8 underflow of PageSegments-1 that would index an empty table.
	p.needsContinue = len(segmentTable) > 0 && segmentTable[len(segmentTable)-1] == 0xFF
	return nil
}

func (p *page) readContent(r io.Reader) error {
	content := make([]byte, p.totalSize)
	_, err := io.ReadFull(r, content)
	if err != nil {
		return noEOF(err)
	}
	checksum := crcUpdate(p.headerChecksum, content)
	if checksum != p.PageChecksum {
		return errors.New("ogg: wrong checksum")
	}
	p.packets = make([][]byte, p.packetCount+1)
	offset := 0
	for i, size := range p.packetSizes {
		p.packets[i] = content[offset : offset+size]
		offset += size
	}
	p.packets[p.packetCount] = content[offset:]
	return nil
}

func noEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
