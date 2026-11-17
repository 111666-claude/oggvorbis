# oggvorbis
a native go ogg/vorbis decoder

[![GoDoc](https://godocs.io/github.com/jfreymuth/oggvorbis?status.svg)](https://godocs.io/github.com/jfreymuth/oggvorbis)

## Usage

This package provides the type oggvorbis.Reader, which can be used to read .ogg files.

	r, err := oggvorbis.NewReader(reader)
	// handle error

	fmt.Println(r.SampleRate())
	fmt.Println(r.Channels())

	buffer := make([]float32, 8192)
	for {
		n, err := r.Read(buffer)

		// use buffer[:n]

		if err == io.EOF {
			break
		}
		if err != nil {
			// handle error
		}
	}

The reader also provides methods for seeking, these will only work if the reader
was created from an io.ReadSeeker.

## Loop points

If a Vorbis file carries `LOOPSTART` and `LOOPEND` comments (sample
coordinates), the reader can loop over that region. Looping requires a
seekable input.

```go
// Infinite loop using the LOOPSTART/LOOPEND comments.
if err := r.SetLoopFromTags(); err != nil {
    // The tags were missing, duplicated, invalid, or out of range.
}

// Or set the region explicitly, either infinite ...
r.SetLoop(start, end)
// ... or for a fixed number of repetitions (playback continues normally
// afterwards).
r.SetLoopCount(start, end, 3)

r.DisableLoop() // stop looping, keeping the current position
```

`LoopPoints()` parses the comments without enabling anything and returns a
distinguishable error for missing, duplicated, inverted, or out-of-range
tags. A non-seekable reader cannot loop; enabling it fails with an error and
plain linear playback is unaffected.

There are also convenience functions to read an entire (small) file, similar to ioutil.ReadAll.

data, format, err := oggvorbis.ReadAll(reader)
