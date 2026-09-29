package api

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
)

// Thin aliases keep the streaming multipart types explicit in server.go while
// using the standard library implementation directly.
type (
	multipartReaderT = multipart.Reader
	multipartPartT   = multipart.Part
)

func multipartNewReader(r io.Reader, boundary string) *multipart.Reader {
	return multipart.NewReader(r, boundary)
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// isMaxBytes reports whether err (or anything it wraps) is the HTTP server's
// body/stream-size error.
func isMaxBytes(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}
