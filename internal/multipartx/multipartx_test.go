package multipartx

import (
	"bytes"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// rawBuilder assembles a multipart body piece by piece.
type rawBuilder struct {
	boundary string
	buf      bytes.Buffer
}

func newRawBuilder(b string) *rawBuilder { return &rawBuilder{boundary: b} }

func (rb *rawBuilder) field(name, ct, body string) *rawBuilder {
	rb.buf.WriteString("--" + rb.boundary + "\r\n")
	rb.buf.WriteString(`Content-Disposition: form-data; name="` + name + `"`)
	if ct != "" {
		rb.buf.WriteString("\r\nContent-Type: " + ct)
	}
	rb.buf.WriteString("\r\n\r\n")
	rb.buf.WriteString(body)
	rb.buf.WriteString("\r\n")
	return rb
}

func (rb *rawBuilder) file(name, filename, ct string, body []byte) *rawBuilder {
	rb.buf.WriteString("--" + rb.boundary + "\r\n")
	rb.buf.WriteString(`Content-Disposition: form-data; name="` + name + `"; filename="` + filename + `"`)
	if ct != "" {
		rb.buf.WriteString("\r\nContent-Type: " + ct)
	}
	rb.buf.WriteString("\r\n\r\n")
	rb.buf.Write(body)
	rb.buf.WriteString("\r\n")
	return rb
}

func (rb *rawBuilder) close() []byte {
	rb.buf.WriteString("--" + rb.boundary + "--\r\n")
	return rb.buf.Bytes()
}

// truncated returns the body with the closing boundary removed, as if the
// client dropped the connection.
func (rb *rawBuilder) truncated() []byte {
	return rb.buf.Bytes()
}

func parse(t *testing.T, body []byte, opts Options) (*Reader, error) {
	t.Helper()
	return NewReader(bytes.NewReader(body), "BOUND", opts)
}

func TestHappyPath(t *testing.T) {
	rb := newRawBuilder("BOUND").
		field("metadata", "application/json", `{"request_id":"r1"}`).
		file("file", "ignored.txt", "text/plain", []byte("hello world"))
	body := rb.close()

	r, err := parse(t, body, Options{MaxFileBytes: 1024, MaxMetaBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := r.ReadMetadata()
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if string(raw) != `{"request_id":"r1"}` {
		t.Fatalf("metadata = %q", raw)
	}
	part, fn, err := r.FilePart()
	if err != nil {
		t.Fatalf("file part: %v", err)
	}
	if fn != "ignored.txt" {
		t.Errorf("filename = %q", fn)
	}
	var sink bytes.Buffer
	n, err := r.StreamFile(part, &sink)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if sink.String() != "hello world" || n != int64(sink.Len()) {
		t.Fatalf("content = %q, n=%d", sink.String(), n)
	}
	if err := r.Finish(part); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestStreamDoesNotBufferWholeFile(t *testing.T) {
	// 4 MiB streamed through a tiny cap on internal buffering: correctness
	// holds and the destination receives every byte exactly once.
	content := make([]byte, 4<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	rb := newRawBuilder("BOUND").
		field("metadata", "application/json", `{"request_id":"big"}`).
		file("file", "big.bin", "", content)
	body := rb.close()

	r, err := parse(t, body, Options{MaxFileBytes: 8 << 20, MaxMetaBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ReadMetadata(); err != nil {
		t.Fatal(err)
	}
	part, _, err := r.FilePart()
	if err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	if _, err := r.StreamFile(part, &sink); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sink.Bytes(), content) {
		t.Fatal("streamed content mismatch")
	}
	if err := r.Finish(part); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestDuplicateField(t *testing.T) {
	rb := newRawBuilder("BOUND").
		field("metadata", "application/json", `{"request_id":"r"}`).
		file("file", "a", "", []byte("aaaa")).
		file("file", "b", "", []byte("bbbb"))
	r, _ := parse(t, rb.close(), Options{MaxFileBytes: 1024, MaxMetaBytes: 1024})
	if _, _, err := r.ReadMetadata(); err != nil {
		t.Fatal(err)
	}
	part, _, err := r.FilePart()
	if err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	if _, err := r.StreamFile(part, &sink); err != nil {
		t.Fatal(err)
	}
	err = r.Finish(part)
	var me *Error
	if !errors.As(err, &me) || me.Kind != KindDuplicateField {
		t.Fatalf("err = %v, want duplicate field", err)
	}
}

func TestUnknownField(t *testing.T) {
	t.Run("unknown first", func(t *testing.T) {
		rb := newRawBuilder("BOUND").
			field("sneaky", "", "x").
			field("metadata", "application/json", `{"request_id":"r"}`)
		r, _ := parse(t, rb.close(), Options{MaxFileBytes: 1024, MaxMetaBytes: 1024})
		_, _, err := r.ReadMetadata()
		var me *Error
		if !errors.As(err, &me) || me.Kind != KindUnknownField {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown after file", func(t *testing.T) {
		rb := newRawBuilder("BOUND").
			field("metadata", "application/json", `{"request_id":"r"}`).
			file("file", "a", "", []byte("aaaa")).
			field("extra", "", "x")
		r, _ := parse(t, rb.close(), Options{MaxFileBytes: 1024, MaxMetaBytes: 1024})
		_, _, _ = r.ReadMetadata()
		part, _, _ := r.FilePart()
		var sink bytes.Buffer
		_, _ = r.StreamFile(part, &sink)
		err := r.Finish(part)
		var me *Error
		if !errors.As(err, &me) || me.Kind != KindUnknownField {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMissingFile(t *testing.T) {
	rb := newRawBuilder("BOUND").
		field("metadata", "application/json", `{"request_id":"r"}`)
	r, _ := parse(t, rb.close(), Options{MaxFileBytes: 1024, MaxMetaBytes: 1024})
	if _, _, err := r.ReadMetadata(); err != nil {
		t.Fatal(err)
	}
	part, _, err := r.FilePart()
	if part != nil {
		part.Close()
	}
	var me *Error
	if !errors.As(err, &me) {
		t.Fatalf("err = %v", err)
	}
}

func TestTruncatedBody(t *testing.T) {
	rb := newRawBuilder("BOUND").
		field("metadata", "application/json", `{"request_id":"r"}`).
		file("file", "a", "", []byte(strings.Repeat("x", 5000)))
	// Cut the body mid-file.
	body := rb.truncated()
	body = body[:len(body)-1000]
	r, _ := parse(t, body, Options{MaxFileBytes: 1 << 20, MaxMetaBytes: 1024})
	if _, _, err := r.ReadMetadata(); err != nil {
		t.Fatal(err)
	}
	part, _, err := r.FilePart()
	if err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	_, err = r.StreamFile(part, &sink)
	var me *Error
	if !errors.As(err, &me) || me.Kind != KindTruncated {
		t.Fatalf("err = %v, want truncated", err)
	}
}

func TestFileTooLarge(t *testing.T) {
	content := []byte(strings.Repeat("z", 11))
	rb := newRawBuilder("BOUND").
		field("metadata", "application/json", `{"request_id":"r"}`).
		file("file", "a", "", content)
	r, _ := parse(t, rb.close(), Options{MaxFileBytes: 10, MaxMetaBytes: 1024})
	_, _, _ = r.ReadMetadata()
	part, _, _ := r.FilePart()
	var sink bytes.Buffer
	_, err := r.StreamFile(part, &sink)
	var me *Error
	if !errors.As(err, &me) || me.Kind != KindFileTooLarge {
		t.Fatalf("err = %v, want too large", err)
	}
}

func TestMissingClosingBoundary(t *testing.T) {
	// Body ends right after a bare (non-closing) boundary delimiter. Go's
	// multipart reader surfaces this as plain io.EOF just like the clean
	// case, so the strict parser must detect it via the raw tail.
	body := []byte("--BOUND\r\n" +
		`Content-Disposition: form-data; name="metadata"` + "\r\n" +
		"Content-Type: application/json\r\n\r\n" +
		`{"request_id":"r"}` + "\r\n" +
		"--BOUND\r\n" +
		`Content-Disposition: form-data; name="file"; filename="f"` + "\r\n\r\n" +
		"data\r\n" +
		"--BOUND\r\n") // no trailing "--"
	r, _ := parse(t, body, Options{MaxFileBytes: 1024, MaxMetaBytes: 1024})
	if _, _, err := r.ReadMetadata(); err != nil {
		t.Fatal(err)
	}
	part, _, err := r.FilePart()
	if err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	if _, err := r.StreamFile(part, &sink); err != nil {
		t.Fatal(err)
	}
	err = r.Finish(part)
	var me *Error
	if !errors.As(err, &me) || me.Kind != KindTruncated {
		t.Fatalf("err = %v, want truncated (missing closing boundary)", err)
	}
}

func TestBoundaryValidation(t *testing.T) {
	if _, err := Boundary("application/json"); err == nil {
		t.Fatal("want error for non-multipart content type")
	}
	if _, err := Boundary("multipart/form-data"); err == nil {
		t.Fatal("want error for missing boundary")
	}
	if _, err := Boundary("multipart/form-data; boundary=abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Guard against accidental regression to http.MaxBytesReader mapping.
func TestClassifyMaxBytes(t *testing.T) {
	base := &http.MaxBytesError{}
	err := classifyRead(base)
	var me *Error
	if !errors.As(err, &me) || me.Kind != KindFileTooLarge {
		t.Fatalf("err = %v", err)
	}
}
