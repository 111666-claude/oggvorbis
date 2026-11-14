package oggvorbis

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/ioutil"
	"testing"
)

// buildPage assembles a single Ogg page from its header fields and content.
func buildPage(flags byte, granule int64, serial, sequence uint32, segmentTable, content []byte) []byte {
	header := make([]byte, 27)
	copy(header, "OggS")
	header[4] = 0
	header[5] = flags
	binary.LittleEndian.PutUint64(header[6:], uint64(granule))
	binary.LittleEndian.PutUint32(header[14:], serial)
	binary.LittleEndian.PutUint32(header[18:], sequence)
	header[26] = byte(len(segmentTable))
	data := append(header, segmentTable...)
	data = append(data, content...)
	// The checksum field is zero while the checksum is computed.
	binary.LittleEndian.PutUint32(data[22:], crcUpdate(0, data))
	return data
}

func TestZeroSegmentPageIsValid(t *testing.T) {
	// A page with no segments and no content is legal framing. It must parse
	// without a panic and be skipped when looking for a packet.
	rest, err := ioutil.ReadFile("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	empty := buildPage(0, 0, 1, 0, nil, nil)
	data := append(empty, rest...)
	format, err := GetFormat(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if format.SampleRate != 44100 {
		t.Errorf("sample rate is %d, expected %d", format.SampleRate, 44100)
	}
	// Its checksum must still be validated.
	corrupted := append([]byte(nil), data...)
	corrupted[5] ^= 0xFF
	if _, err := GetFormat(bytes.NewReader(corrupted)); err == nil {
		t.Error("corrupted empty page: expected a checksum error")
	}
}

func TestReadCleanEOFFromPageBoundary(t *testing.T) {
	// A stream that simply stops at a page boundary (without an EndOfStream
	// flag) must end with io.EOF, not io.ErrUnexpectedEOF.
	data, err := ioutil.ReadFile("testdata/test.ogg")
	if err != nil {
		t.Fatal(err)
	}
	r := &oggReader{source: bytes.NewReader(data)}
	pages := 0
	for {
		_, err := r.NextPacket()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("packet %d: unexpected error %v", pages, err)
		}
		pages++
		if pages > 1000 {
			t.Fatal("too many packets without EOF")
		}
	}
	if pages == 0 {
		t.Fatal("no packets read")
	}
}
