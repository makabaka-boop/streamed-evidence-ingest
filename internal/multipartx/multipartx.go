// Package multipartx is a strict, streaming multipart/form-data reader for
// the upload endpoint. It enforces:
//
//   - exactly one "metadata" part and exactly one "file" part, in that
//     order, with no extra parts or fields;
//   - hard byte limits on the metadata and on the file content;
//   - detection of truncated requests (missing part data or missing
//     closing boundary).
//
// The file part is handed to the caller as a stream; it is never buffered
// in memory.
package multipartx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

// Kind classifies a parse error so the HTTP layer can pick a status code.
type Kind int

const (
	KindMalformed Kind = iota
	KindTruncated
	KindDuplicateField
	KindUnknownField
	KindMetadataTooLarge
	KindFileTooLarge
)

// Error is a typed multipart parse error.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return "multipart: " + e.Msg }

func kerr(kind Kind, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

const copyBufSize = 32 * 1024

// Options caps the two parts.
type Options struct {
	MaxFileBytes int64
	MaxMetaBytes int64
}

// Reader walks a multipart body in strict order.
type Reader struct {
	mr       *multipart.Reader
	opts     Options
	boundary string
	tail     *tailReader
	gotMeta  bool
	gotFile  bool
}

// ContentType extracts and validates the multipart boundary.
func Boundary(contentType string) (string, error) {
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", kerr(KindMalformed, "invalid Content-Type: %v", err)
	}
	if media != "multipart/form-data" {
		return "", kerr(KindMalformed, "expected multipart/form-data, got %q", media)
	}
	boundary, ok := params["boundary"]
	if !ok || boundary == "" {
		return "", kerr(KindMalformed, "missing multipart boundary")
	}
	return boundary, nil
}

// NewReader constructs a strict reader over body.
func NewReader(body io.Reader, boundary string, opts Options) (*Reader, error) {
	// Room for "--boundary--" plus its trailing CRLF, so the retained
	// window still contains the full delimiter at clean EOF.
	tail := newTailReader(body, len("--")+len(boundary)+len("--")+2)
	return &Reader{
		mr:       multipart.NewReader(tail, boundary),
		opts:     opts,
		boundary: boundary,
		tail:     tail,
	}, nil
}

// tailReader retains only the last n bytes ever read, using a fixed-size
// ring. Its memory is O(n) regardless of body size, so it never causes a
// large upload to be retained in memory. It lets the parser prove it
// actually consumed the multipart closing delimiter ("--boundary--")
// rather than merely hitting EOF after a bare boundary line.
type tailReader struct {
	r    io.Reader
	buf  []byte
	size int
}

func newTailReader(r io.Reader, n int) *tailReader {
	return &tailReader{r: r, buf: make([]byte, n)}
}

func (t *tailReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		switch {
		case n >= len(t.buf):
			copy(t.buf, p[n-len(t.buf):n])
			t.size = len(t.buf)
		default:
			// Shift out the oldest bytes to make room, then append.
			if t.size+n > len(t.buf) {
				drop := t.size + n - len(t.buf)
				copy(t.buf, t.buf[drop:t.size])
				t.size -= drop
			}
			copy(t.buf[t.size:], p[:n])
			t.size += n
		}
	}
	return n, err
}

// sawClosing reports whether the retained tail ends with closingDelim
// (ignoring trailing CRLF, which the caller strips from the retained bytes
// conceptually; the delimiter carries no CRLF).
func (t *tailReader) sawClosing(closingDelim string) bool {
	kept := t.buf[:t.size]
	cl := []byte(closingDelim)
	end := t.size
	for end > 0 && (kept[end-1] == '\r' || kept[end-1] == '\n') {
		end--
	}
	return end >= len(cl) && bytes.Equal(kept[end-len(cl):end], cl)
}

// partName parses a part's Content-Disposition and returns the field name
// and optional client filename.
func partName(h textproto.MIMEHeader) (name, filename string, err error) {
	disp := h.Get("Content-Disposition")
	if disp == "" {
		return "", "", kerr(KindMalformed, "part is missing Content-Disposition")
	}
	media, params, err := mime.ParseMediaType(disp)
	if err != nil {
		return "", "", kerr(KindMalformed, "invalid Content-Disposition: %v", err)
	}
	if media != "form-data" {
		return "", "", kerr(KindMalformed, "unexpected Content-Disposition %q", media)
	}
	name = params["name"]
	filename = params["filename"]
	return name, filename, nil
}

// ReadMetadata reads the first part, which must be the JSON metadata.
// It reads at most MaxMetaBytes and never returns partial metadata on
// error.
func (r *Reader) ReadMetadata() ([]byte, textproto.MIMEHeader, error) {
	part, err := r.nextPart()
	if err != nil {
		return nil, nil, err
	}
	name, filename, err := partName(part.Header)
	if err != nil {
		return nil, nil, err
	}
	if name != "metadata" {
		return nil, nil, kerr(KindUnknownField, "first part must be %q, got %q", "metadata", name)
	}
	if filename != "" {
		return nil, nil, kerr(KindMalformed, "metadata part must not carry a filename")
	}
	if ct := part.Header.Get("Content-Type"); ct != "" {
		media, _, perr := mime.ParseMediaType(ct)
		if perr != nil || media != "application/json" {
			return nil, nil, kerr(KindMalformed, "metadata part must be application/json, got %q", ct)
		}
	}
	r.gotMeta = true

	lr := io.LimitReader(part, r.opts.MaxMetaBytes+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, nil, classifyRead(err)
	}
	if int64(len(data)) > r.opts.MaxMetaBytes {
		return nil, nil, kerr(KindMetadataTooLarge, "metadata exceeds %d bytes", r.opts.MaxMetaBytes)
	}
	// Reading to EOF inside a part leaves the reader positioned at the
	// boundary. Part.Close drains anything left (there should be nothing).
	if err := part.Close(); err != nil {
		return nil, nil, mapTrailing(err)
	}
	return data, part.Header, nil
}

// FilePart returns the second part, which must be the file field.
func (r *Reader) FilePart() (*multipart.Part, string, error) {
	if !r.gotMeta {
		return nil, "", kerr(KindMalformed, "metadata must precede file")
	}
	part, err := r.nextPart()
	if err != nil {
		return nil, "", err
	}
	name, filename, err := partName(part.Header)
	if err != nil {
		return nil, "", err
	}
	if name == "metadata" {
		return nil, "", kerr(KindDuplicateField, "duplicate %q field", "metadata")
	}
	if name != "file" {
		return nil, "", kerr(KindUnknownField, "unknown field %q", name)
	}
	r.gotFile = true
	return part, filename, nil
}

// StreamFile copies exactly the file part's bytes to dst, computing no
// hash itself (the caller tees a hash in), enforcing the size limit and
// mapping truncated-body errors.
func (r *Reader) StreamFile(part io.Reader, dst io.Writer) (int64, error) {
	lr := io.LimitReader(part, r.opts.MaxFileBytes+1)
	buf := make([]byte, copyBufSize)
	var n int64
	for {
		nr, er := lr.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw < 0 || nr < nw {
				nw = 0
			}
			n += int64(nw)
			if ew != nil {
				return n, ew
			}
			if nr != nw {
				return n, io.ErrShortWrite
			}
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				break
			}
			return n, classifyRead(er)
		}
	}
	if n > r.opts.MaxFileBytes {
		return n, kerr(KindFileTooLarge, "file exceeds %d bytes", r.opts.MaxFileBytes)
	}
	return n, nil
}

// Finish must be called after the file part has been fully consumed.
// It verifies there are no extra parts and that the multipart body ends
// with the proper closing boundary.
func (r *Reader) Finish(file io.Closer) error {
	if file != nil {
		if err := file.Close(); err != nil {
			return mapTrailing(err)
		}
	}
	if !r.gotFile {
		return kerr(KindMalformed, "missing %q field", "file")
	}
	part, err := r.mr.NextPart()
	switch {
	case err == nil:
		name, _, perr := partName(part.Header)
		if perr != nil {
			return perr
		}
		switch name {
		case "metadata":
			return kerr(KindDuplicateField, "duplicate %q field", "metadata")
		case "file":
			return kerr(KindDuplicateField, "duplicate %q field", "file")
		default:
			return kerr(KindUnknownField, "unknown field %q", name)
		}
	case errors.Is(err, io.EOF):
		// NextPart returns io.EOF both for a proper closing delimiter and
		// for EOF right after a bare (non-closing) boundary. Require proof
		// in the raw tail that the body actually ended with "--boundary--".
		if r.tail.sawClosing("--" + r.boundary + "--") {
			return nil
		}
		return kerr(KindTruncated, "request body truncated: missing multipart closing boundary")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return kerr(KindTruncated, "request body truncated before closing boundary")
	default:
		return kerr(KindMalformed, "reading multipart trailer: %v", err)
	}
}

func (r *Reader) nextPart() (*multipart.Part, error) {
	part, err := r.mr.NextPart()
	switch {
	case err == nil:
		return part, nil
	case errors.Is(err, io.EOF):
		return nil, kerr(KindMalformed, "expected another part but body ended")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return nil, kerr(KindTruncated, "request body truncated inside multipart stream")
	default:
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			return nil, kerr(KindFileTooLarge, "request body exceeds the permitted size")
		}
		msg := err.Error()
		if strings.Contains(msg, "boundary") {
			return nil, kerr(KindMalformed, "%s", msg)
		}
		return nil, kerr(KindMalformed, "reading multipart body: %v", err)
	}
}

func mapTrailing(err error) error {
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return kerr(KindTruncated, "request body truncated")
	case errors.Is(err, io.EOF):
		return nil
	default:
		return kerr(KindMalformed, "multipart stream error: %v", err)
	}
}

// classifyRead maps a body read error to the correct failure kind. A body
// cut off mid-stream is a truncation; hitting the server-side body cap is
// an oversized request.
func classifyRead(err error) error {
	var mbErr *http.MaxBytesError
	if errors.As(err, &mbErr) {
		return kerr(KindFileTooLarge, "request body exceeds the permitted size")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return kerr(KindTruncated, "file part truncated")
	}
	return kerr(KindMalformed, "reading file part: %v", err)
}
