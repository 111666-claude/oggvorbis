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

For seekable inputs a sparse page index is built when the reader is created, so
repeated SetPosition calls don't rescan the stream from the beginning. The
index interval (in samples per channel) can be configured — or the index
disabled with a non-positive interval — via:

	r, err := oggvorbis.NewReaderWithSeekIndex(reader, interval)

If the stream cannot be indexed (chained streams, invalid granule positions)
or changes after the index was built, seeking degrades to the old linear scan.

There are also convenience functions to read an entire (small) file, similar to ioutil.ReadAll.

	data, format, err := oggvorbis.ReadAll(reader)
