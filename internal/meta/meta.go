// Package meta parses the JSON metadata part of an upload request.
//
// Parsing is deliberately strict:
//   - the payload must be a single JSON object with no trailing data;
//   - only the documented fields are accepted (unknown fields fail);
//   - a repeated key anywhere in the document fails (encoding/json silently
//     keeps the last value, so a duplicate-key scan runs first).
package meta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"strings"
)

// MaxBytes bounds the metadata part. It is small on purpose.
const MaxBytes = 1 << 20 // 1 MiB

// Metadata is the accepted JSON document for an upload.
type Metadata struct {
	// RequestID is the client-supplied idempotency key. It is the only
	// identifier the service exposes; the client filename never reaches
	// the storage layer.
	RequestID string `json:"request_id"`

	// ContentType optionally declares the media type stored with the
	// blob and served back from the content endpoint.
	ContentType string `json:"content_type"`
}

// Parse validates and decodes one metadata document from r.
func Parse(r io.Reader) (Metadata, error) {
	// ReadAll never sees an unbounded stream: callers cap the part at
	// MaxBytes before invoking Parse.
	raw, err := io.ReadAll(r)
	if err != nil {
		return Metadata{}, err
	}
	if len(raw) == 0 {
		return Metadata{}, fmt.Errorf("metadata is empty")
	}

	if err := scanDuplicates(bytes.NewReader(raw)); err != nil {
		return Metadata{}, err
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Metadata
	if err := dec.Decode(&m); err != nil {
		return Metadata{}, fmt.Errorf("invalid metadata JSON: %w", err)
	}
	// Decode ignores data after the first value; forbid trailing junk.
	if dec.More() {
		return Metadata{}, fmt.Errorf("invalid metadata JSON: unexpected trailing data")
	}

	if err := validate(&m); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

func validate(m *Metadata) error {
	id := strings.TrimSpace(m.RequestID)
	if id == "" {
		return fmt.Errorf("request_id is required")
	}
	if len(id) > 200 {
		return fmt.Errorf("request_id too long (max 200 bytes)")
	}
	if strings.ContainsAny(id, "/?#%") {
		return fmt.Errorf("request_id contains forbidden characters")
	}
	m.RequestID = id

	ct := strings.TrimSpace(m.ContentType)
	if ct != "" {
		mediaType, params, err := mime.ParseMediaType(ct)
		if err != nil {
			return fmt.Errorf("content_type is not a valid media type: %w", err)
		}
		if len(params) > 0 {
			return fmt.Errorf("content_type must not include parameters")
		}
		if mediaType == "" || strings.HasPrefix(mediaType, "/") || strings.HasSuffix(mediaType, "/") {
			return fmt.Errorf("content_type is malformed")
		}
		m.ContentType = mediaType
	}
	return nil
}

// scanDuplicates walks the JSON tokens and fails if any object repeats a
// key. encoding/json would otherwise silently keep the last value.
func scanDuplicates(r io.Reader) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid metadata JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("metadata must be a JSON object")
	}
	return scanObject(dec)
}

func scanObject(dec *json.Decoder) error {
	seen := map[string]struct{}{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("invalid metadata JSON: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("invalid metadata JSON: expected object key")
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("metadata contains duplicate field %q", key)
		}
		seen[key] = struct{}{}
		if err := scanValue(dec); err != nil {
			return err
		}
	}
	// consume closing brace
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("invalid metadata JSON: %w", err)
	}
	return nil
}

func scanArray(dec *json.Decoder) error {
	for dec.More() {
		if err := scanValue(dec); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("invalid metadata JSON: %w", err)
	}
	return nil
}

func scanValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid metadata JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			return scanObject(dec)
		case '[':
			return scanArray(dec)
		default:
			return fmt.Errorf("invalid metadata JSON: unexpected %q", d)
		}
	}
	return nil
}

// HasJSONContentType reports whether the part's Content-Type is acceptable
// for the metadata part. Missing or application/json is accepted.
func HasJSONContentType(ct string) bool {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}
