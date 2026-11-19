package oggvorbis

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
)

type oggReader struct {
	source io.Reader
	seeker io.Seeker
	buffer []byte

	currentPage      page
	lastPagePosition int64
	packetIndex      int
	ready            bool
	lastPacket       bool

	index     *seekIndex // nil: no index, SeekPageBefore scans from the start
	seekScans int        // pages inspected by the most recent SeekPageBefore
}

func (r *oggReader) NextPacket() ([]byte, error) {
	if !r.ready {
		err := r.currentPage.read(r)
		if err != nil {
			return nil, err
		}
		r.packetIndex = 0
		if r.currentPage.HeaderTypeFlag&headerFlagContinuedPacket != 0 {
			r.packetIndex = 1
		}
		r.ready = true
	}
	if r.packetIndex == r.currentPage.packetCount {
		rest := r.currentPage.packets[r.currentPage.packetCount]
		if r.currentPage.AbsoluteGranulePosition != -1 {
			r.lastPagePosition = r.currentPage.AbsoluteGranulePosition
		}
		err := r.currentPage.read(r)
		if err != nil {
			return nil, err
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
		r.lastPacket = true
	}
	return packet, nil
}

func (r *oggReader) Restore() error {
	buffer := make([]byte, 1024)
	for {
		b := buffer
		n, err := io.ReadAtLeast(r, b, 4)
		if err != nil {
			return err
		}
		i := bytes.Index(b[:n], capturePattern[:])
		if i == -1 {
			r.buffer = b[n-3 : n]
			continue
		}
		r.buffer = b[i:n]
		r.ready = false
		return nil
	}
}

func (r *oggReader) LastPosition() (int64, error) {
	_, err := r.seeker.Seek(-maxPageSize, io.SeekEnd)
	if err != nil {
		r.seeker.Seek(0, io.SeekStart)
	}
	r.buffer = nil
	if err := r.Restore(); err != nil {
		return 0, err
	}
	var p page
	var result int64
	for {
		err := p.readHeader(r)
		if err == io.EOF {
			return result, nil
		} else if err != nil {
			return result, err
		}
		result = p.AbsoluteGranulePosition
		if p.isLast() {
			return result, nil
		}
		r.seeker.Seek(int64(p.totalSize-len(r.buffer)), io.SeekCurrent)
		r.buffer = nil
	}
}

// SeekPageBefore positions the reader at the last page whose granule position
// is <= pos and returns that page's granule position. When a seek index is
// available the forward scan starts at the nearest indexed anchor instead of
// at the beginning of the stream.
func (r *oggReader) SeekPageBefore(pos int64) (int64, error) {
	from, fromGranule := int64(0), int64(0)
	if r.index != nil {
		if offset, granule, ok := r.index.lookup(pos); ok {
			if r.validPageOffset(offset) {
				from, fromGranule = offset, granule
			} else {
				// The indexed offset no longer holds a page capture
				// pattern: the stream changed since the index was built.
				// Drop the index and degrade to a full scan.
				r.index = nil
			}
		}
	}
	return r.seekPageBefore(pos, from, fromGranule)
}

// validPageOffset reports whether a page capture pattern is present at the
// given file offset. It is used to detect streams that changed after the
// seek index was built.
func (r *oggReader) validPageOffset(offset int64) bool {
	if offset == 0 {
		return true
	}
	if _, err := r.seeker.Seek(offset, io.SeekStart); err != nil {
		return false
	}
	var magic [4]byte
	if _, err := io.ReadFull(r.source, magic[:]); err != nil {
		return false
	}
	return magic == capturePattern
}

func (r *oggReader) seekPageBefore(pos, from, fromGranule int64) (int64, error) {
	r.seeker.Seek(from, io.SeekStart)
	r.buffer = nil
	r.lastPacket = false
	r.seekScans = 0
	var p page
	lastOffset := from
	lastPos := fromGranule
	prevGranule := fromGranule
	for {
		offset, _ := r.seeker.Seek(0, io.SeekCurrent)
		err := p.readHeader(r)
		if err != nil {
			return 0, err
		}
		r.seekScans++
		g := p.AbsoluteGranulePosition
		if g >= 0 {
			if g < prevGranule {
				return 0, errBadGranule
			}
			prevGranule = g
		}
		if g > pos {
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
		// Pages with granule -1 contain no completed packet (a packet
		// spans the whole page); they cannot serve as a seek target.
		// Skipping them lets decoding resume at the previous page and
		// reassemble the continued packet instead of dropping it.
		if g >= 0 {
			lastOffset = offset
			lastPos = g
		}
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
