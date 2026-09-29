package meta

import (
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		in string
		id string
		ct string
	}{
		{`{"request_id":"abc"}`, "abc", ""},
		{`{"request_id":"abc","content_type":"image/png"}`, "abc", "image/png"},
		{`{ "request_id" : "x" }`, "x", ""},
	}
	for _, c := range cases {
		m, err := Parse(strings.NewReader(c.in))
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if m.RequestID != c.id {
			t.Fatalf("id = %q want %q", m.RequestID, c.id)
		}
		if m.ContentType != c.ct {
			t.Fatalf("ct = %q want %q", m.ContentType, c.ct)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		``,
		`{}`,
		`{"request_id":""}`,
		`{"request_id":"a/b"}`,
		`{"request_id":"x","unknown":1}`,
		`{"request_id":"x","request_id":"y"}`,
		`{"request_id":"x","nested":{"a":1,"a":2}}`,
		`not json`,
		`[1,2,3]`,
		`{"request_id":"x"} garbage`,
		`{"request_id":"x","content_type":"text/plain; charset=utf-8"}`,
		`{"request_id":"x","content_type":"not a type"}`,
	}
	for _, in := range bad {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestContentTypeNormalized(t *testing.T) {
	m, err := Parse(strings.NewReader(`{"request_id":"x","content_type":"  Text/PLAIN "}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.ContentType != "text/plain" {
		t.Fatalf("got %q", m.ContentType)
	}
}
