// Package metadata decodes the upload request's JSON metadata using a
// strict decoder: unknown fields, duplicate object keys, trailing data and
// multiple JSON values all reject the whole request.
package metadata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

// maxNesting guards deeply nested JSON used to abuse the duplicate-key walk.
const maxNesting = 32

// Metadata is the single permitted JSON document for an upload.
type Metadata struct {
	// RequestID is the client-supplied idempotency/deduplication key.
	RequestID string `json:"request_id"`
	// MediaType is the declared content type; optional.
	MediaType string `json:"media_type,omitempty"`
}

var requestIDRE = regexp.MustCompile(`^[\x21-\x7E]{1,200}$`)

// Decode parses and validates one JSON object.
func Decode(raw []byte) (Metadata, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Metadata{}, fmt.Errorf("metadata is empty")
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return Metadata{}, err
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Metadata
	if err := dec.Decode(&m); err != nil {
		return Metadata{}, fmt.Errorf("invalid metadata JSON: %v", err)
	}
	// Exactly one JSON document: a second value means trailing junk.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		return Metadata{}, fmt.Errorf("metadata has trailing data after the JSON object")
	} else if err != io.EOF {
		return Metadata{}, fmt.Errorf("invalid metadata JSON: %v", err)
	}

	m.RequestID = trimBytes(m.RequestID)
	if !requestIDRE.MatchString(m.RequestID) {
		return Metadata{}, fmt.Errorf("request_id must be 1-200 printable non-space ASCII characters")
	}
	// A request id is not a path, but keep separators out anyway so it can
	// never be misused in a filesystem context.
	if containsAny(m.RequestID, `/\`) {
		return Metadata{}, fmt.Errorf("request_id must not contain path separators")
	}
	if len(m.MediaType) > 255 {
		return Metadata{}, fmt.Errorf("media_type too long")
	}
	if hasControl(m.MediaType) {
		return Metadata{}, fmt.Errorf("media_type contains control characters")
	}
	return m, nil
}

// rejectDuplicateKeys walks raw JSON and fails if any object defines the
// same key twice at the same level.
func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func(depth int) error
	walk = func(depth int) error {
		if depth > maxNesting {
			return fmt.Errorf("metadata nested too deeply")
		}
		t, err := dec.Token()
		if err != nil {
			return fmt.Errorf("invalid metadata JSON: %v", err)
		}
		switch d := t.(type) {
		case json.Delim:
			switch d {
			case '{':
				seen := map[string]struct{}{}
				for dec.More() {
					kt, err := dec.Token() // key
					if err != nil {
						return fmt.Errorf("invalid metadata JSON: %v", err)
					}
					key, ok := kt.(string)
					if !ok {
						return fmt.Errorf("invalid metadata JSON: expected object key")
					}
					if _, dup := seen[key]; dup {
						return fmt.Errorf("duplicate JSON key %q", key)
					}
					seen[key] = struct{}{}
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
				if _, err := dec.Token(); err != nil { // closing '}'
					return fmt.Errorf("invalid metadata JSON: %v", err)
				}
			case '[':
				for dec.More() {
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
				if _, err := dec.Token(); err != nil { // closing ']'
					return fmt.Errorf("invalid metadata JSON: %v", err)
				}
			default:
				return fmt.Errorf("invalid metadata JSON: unexpected delimiter %q", d)
			}
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	return nil
}

func trimBytes(s string) string {
	return string(bytes.TrimSpace([]byte(s)))
}

func containsAny(s, chars string) bool {
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] {
				return true
			}
		}
	}
	return false
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}
