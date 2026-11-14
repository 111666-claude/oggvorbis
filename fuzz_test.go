package oggvorbis

import (
	"bytes"
	"testing"
)

func TestFuzzCrashers(t *testing.T) {
	testData := []string{
		"\xff\xff\xff\xff\xff\xff\xc9\x03",
		// Page header declaring more segment-table bytes than are present.
		"OggS\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01",
		// Ogg page header declaring zero segments: the segment table is
		// empty and must not be indexed with PageSegments-1.
		"OggS\x00000000000000000000000\x00",
	}

	for _, s := range testData {
		b := bytes.NewReader([]byte(s))
		_, _ = NewReader(b)
	}
}
