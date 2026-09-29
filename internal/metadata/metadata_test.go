package metadata

import (
	"strings"
	"testing"
)

func TestDecodeOK(t *testing.T) {
	cases := []struct {
		name string
		in   string
		rid  string
		mt   string
	}{
		{"minimal", `{"request_id":"abc"}`, "abc", ""},
		{"both", `{"request_id":"r1","media_type":"text/plain"}`, "r1", "text/plain"},
		{"spaces trimmed", `{"request_id":"  r1  "}`, "r1", ""},
		{"order swapped", `{"media_type":"a/b","request_id":"z"}`, "z", "a/b"},
		{"empty file is allowed", `{"request_id":"x"}`, "x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Decode([]byte(tc.in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m.RequestID != tc.rid {
				t.Errorf("request_id = %q, want %q", m.RequestID, tc.rid)
			}
			if m.MediaType != tc.mt {
				t.Errorf("media_type = %q, want %q", m.MediaType, tc.mt)
			}
		})
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring expected in the error
	}{
		{"empty", ``, "empty"},
		{"blank", "   \n ", "empty"},
		{"not json", `{"request_id":}`, "invalid metadata JSON"},
		{"unknown field", `{"request_id":"a","evil":1}`, "unknown field"},
		{"duplicate key", `{"request_id":"a","request_id":"b"}`, "duplicate JSON key"},
		{"duplicate nested key", `{"request_id":"a","o":{"x":1,"x":2}}`, "duplicate JSON key"},
		{"trailing data", `{"request_id":"a"} {"request_id":"b"}`, "trailing data"},
		{"array", `["request_id","a"]`, "cannot unmarshal"},
		{"missing request id", `{"media_type":"a/b"}`, "request_id"},
		{"empty request id", `{"request_id":""}`, "request_id"},
		{"request id slash", `{"request_id":"a/b"}`, "separator"},
		{"request id backslash", `{"request_id":"a\\b"}`, "separator"},
		{"request id space inside", `{"request_id":"a b"}`, "printable"},
		{"request id too long", `{"request_id":"` + strings.Repeat("a", 201) + `"}`, "printable"},
		{"media type control", `{"request_id":"a","media_type":"a\tb"}`, "control"},
		{"null request id", `{"request_id":null}`, "request_id"},
		{"numeric request id", `{"request_id":42}`, "cannot unmarshal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
