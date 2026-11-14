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

// restoreBufferSize is the maximum amount of input Restore reads at a time.
const restoreBufferSize = 1024

// Restore advances the reader until the next page capture pattern is found.
//
// On success the buffered data starts with the capture pattern, so the page
// header, segment table and packet data that may have been read together
// with the pattern are not lost.
//
// On failure at most len(capturePattern)-1 trailing bytes that could still
// be the beginning of a capture pattern are kept, so a later call reports the
// same result instead of re-reading input that was already consumed. Restore
// always makes progress: it returns io.EOF for empty input, io.ErrUnexpectedEOF
// when the input ends in the middle of a possible pattern, io.ErrNoProgress
// for a source that neither returns data nor an error, and the underlying
// error otherwise.
func (r *oggReader) Restore() error {
	r.ready = false
	r.lastPacket = false

	buf := make([]byte, restoreBufferSize)
	n := 0

	// Examine any bytes left over from a previous call first. Anything
	// before a pattern is garbage, and without a pattern only the final
	// bytes can still be the prefix of one.
	if len(r.buffer) > 0 {
		if i := bytes.Index(r.buffer, capturePattern[:]); i >= 0 {
			r.buffer = append([]byte(nil), r.buffer[i:]...)
			return nil
		}
		keep := len(r.buffer)
		if keep > len(capturePattern)-1 {
			keep = len(capturePattern) - 1
		}
		n = copy(buf, r.buffer[len(r.buffer)-keep:])
		r.buffer = nil
	}

	var readErr error
	for {
		if i := bytes.Index(buf[:n], capturePattern[:]); i >= 0 {
			// Keep everything from the pattern onwards.
			r.buffer = append([]byte(nil), buf[i:n]...)
			return nil
		}

		// Only a partial pattern at the end can matter for the next
		// attempt; discarding the rest keeps the buffer bounded.
		keep := n
		if keep > len(capturePattern)-1 {
			keep = len(capturePattern) - 1
		}
		copy(buf, buf[n-keep:n])
		n = keep

		if readErr != nil {
			r.buffer = append([]byte(nil), buf[:n]...)
			if readErr == io.EOF && n > 0 {
				// The input ended in the middle of a possible
				// capture pattern.
				return io.ErrUnexpectedEOF
			}
			return readErr
		}

		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			readErr = err
		}
		if m == 0 && err == nil {
			// A source that returns neither data nor an error
			// would otherwise turn this loop into a busy wait.
			r.buffer = append([]byte(nil), buf[:n]...)
			return io.ErrNoProgress
		}
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
		if err != nil {
			return result, err
		}
		result = p.AbsoluteGranulePosition
		if p.isLast() {
			break
		}
		r.seeker.Seek(int64(p.totalSize-len(r.buffer)), io.SeekCurrent)
		r.buffer = nil
	}
	return result, nil
}

func (r *oggReader) SeekPageBefore(pos int64) (int64, error) {
	r.seeker.Seek(0, io.SeekStart)
	r.buffer = nil
	r.lastPacket = false
	var p page
	var lastOffset int64
	var lastPos int64
	for {
		offset, _ := r.seeker.Seek(0, io.SeekCurrent)
		err := p.readHeader(r)
		if err != nil {
			return 0, err
		}
		if p.AbsoluteGranulePosition > pos {
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
		return err
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
	p.needsContinue = segmentTable[p.PageSegments-1] == 0xFF
	return nil
}

func (p *page) readContent(r io.Reader) error {
	content := make([]byte, p.totalSize)
	_, err := io.ReadFull(r, content)
	if err != nil {
		return err
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
