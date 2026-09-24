package export

import (
	"archive/zip"
	"hash/crc32"
	"time"
)

// WriteStoredEntry writes data to zw as one stored (uncompressed) entry named name with
// its size and CRC in the local header and no data descriptor, like the images of
// export.zip (see writeStored), modified at modified. The studio job
// (internal/pipeline/studiojob) builds studio.zip with it.
func WriteStoredEntry(zw *zip.Writer, name string, data []byte, modified time.Time) error {
	size := uint64(len(data))
	fh := &zip.FileHeader{
		Name:               name,
		Method:             zip.Store,
		CRC32:              crc32.ChecksumIEEE(data),
		CompressedSize64:   size,
		UncompressedSize64: size,
	}
	fh.SetMode(0o644)
	setRawHeaderDefaults(fh, modified)
	w, err := zw.CreateRaw(fh)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
