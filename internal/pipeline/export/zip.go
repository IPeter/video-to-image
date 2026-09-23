package export

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"time"

	"video-to-image/internal/storage"
)

// errReaderGone is what the ZIP writer sees once the storage Put has returned and the
// read side of the pipe is closed.
var errReaderGone = errors.New("export: zip upload stopped reading")

// putZip streams the archive into st.Put through an io.Pipe. The writer goroutine
// always finishes before putZip returns: a failing writer closes the pipe with its
// error (so Put fails), a returning Put closes the read side (so a blocked Write
// fails), and ctx cancellation closes the write side.
func putZip(ctx context.Context, st storage.Storage, key string, items []item, manifest []byte, modified time.Time) error {
	pr, pw := io.Pipe()
	stop := context.AfterFunc(ctx, func() { pw.CloseWithError(ctx.Err()) })
	defer stop()

	writeDone := make(chan error, 1)
	go func() {
		err := writeZip(ctx, pw, items, manifest, modified)
		pw.CloseWithError(err) // nil: the reader sees io.EOF
		writeDone <- err
	}()

	putErr := st.Put(ctx, key, pr, "application/zip")
	pr.CloseWithError(errReaderGone)
	writeErr := <-writeDone

	switch {
	case putErr == nil && writeErr == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("export: zip: %w", ctx.Err())
	case writeErr != nil && !errors.Is(writeErr, errReaderGone):
		// The writer failed first; Put only saw the error through the pipe.
		return fmt.Errorf("export: write zip: %w", writeErr)
	case putErr != nil:
		return fmt.Errorf("export: store zip: %w", putErr)
	default:
		return errors.New("export: store zip: storage returned before reading the whole archive")
	}
}

// writeZip writes every image as a stored entry, then manifest.json deflated.
func writeZip(ctx context.Context, w io.Writer, items []item, manifest []byte, modified time.Time) error {
	zw := zip.NewWriter(w)
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeStored(ctx, zw, it, modified); err != nil {
			return fmt.Errorf("%s: %w", it.FileName, err)
		}
	}
	fh := &zip.FileHeader{Name: ManifestFile, Method: zip.Deflate, Modified: modified}
	fh.SetMode(0o644)
	mw, err := zw.CreateHeader(fh)
	if err != nil {
		return fmt.Errorf("%s: %w", ManifestFile, err)
	}
	if _, err := mw.Write(manifest); err != nil {
		return fmt.Errorf("%s: %w", ManifestFile, err)
	}
	return zw.Close()
}

// writeStored copies one JPEG (the uploaded bytes, it.path) uncompressed. Size and CRC are known from the upload
// pass, so they go into the local header and no data descriptor follows: some
// streaming unzippers (Java's ZipInputStream among them) reject stored entries with a
// descriptor. The copy re-checks both, failing if the file changed in between.
func writeStored(ctx context.Context, zw *zip.Writer, it item, modified time.Time) error {
	f, err := os.Open(it.path)
	if err != nil {
		return err
	}
	defer f.Close()

	size := uint64(it.Bytes)
	fh := &zip.FileHeader{
		Name:               it.FileName,
		Method:             zip.Store,
		CRC32:              it.crc,
		CompressedSize64:   size,
		UncompressedSize64: size,
	}
	fh.SetMode(0o644)
	setRawHeaderDefaults(fh, modified)
	ew, err := zw.CreateRaw(fh)
	if err != nil {
		return err
	}
	h := crc32.NewIEEE()
	n, err := io.Copy(io.MultiWriter(ew, h), io.LimitReader(ctxReader{ctx: ctx, r: f}, it.Bytes+1))
	if err != nil {
		return err
	}
	if n != it.Bytes || h.Sum32() != it.crc {
		return fmt.Errorf("%s changed after upload", it.path)
	}
	return nil
}

// setRawHeaderDefaults fills what zip.Writer.CreateHeader would and CreateRaw does
// not: version fields and the modification time (MS-DOS fields plus the Info-ZIP
// extended timestamp that readers prefer).
func setRawHeaderDefaults(fh *zip.FileHeader, modified time.Time) {
	const zipVersion20 = 20 // 2.0
	fh.CreatorVersion = fh.CreatorVersion&0xff00 | zipVersion20
	fh.ReaderVersion = zipVersion20

	t := modified.UTC()
	fh.Modified = t
	if t.Year() < 1980 {
		t = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	// CreateRaw (unlike CreateHeader) does not derive the MS-DOS fields from Modified,
	// so the deprecated fields must be set by hand here.
	fh.ModifiedDate = uint16(t.Day() + int(t.Month())<<5 + (t.Year()-1980)<<9) //nolint:staticcheck // see above
	fh.ModifiedTime = uint16(t.Second()/2 + t.Minute()<<5 + t.Hour()<<11)      //nolint:staticcheck // see above

	const extTimeExtraID = 0x5455
	var extra [9]byte
	binary.LittleEndian.PutUint16(extra[0:], extTimeExtraID)
	binary.LittleEndian.PutUint16(extra[2:], 5) // flags byte + mtime
	extra[4] = 1                                // mtime present
	binary.LittleEndian.PutUint32(extra[5:], uint32(modified.Unix()))
	fh.Extra = append(fh.Extra, extra[:]...)
}

// ctxReader stops a copy once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
