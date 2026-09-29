package multipartx

import (
	"strings"
	"testing"
)

func TestTailReaderKeepsLastN(t *testing.T) {
	tr := newTailReader(strings.NewReader("abcdefghij"), 4)
	buf := make([]byte, 3)
	var got []byte
	for {
		n, err := tr.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if string(tr.buf[:tr.size]) != "ghij" {
		t.Fatalf("retained = %q, want ghij", tr.buf[:tr.size])
	}
	// Sanity: everything was delivered to the consumer.
	if string(got) != "abcdefghij" {
		t.Fatalf("delivered = %q", got)
	}
}

func TestSawClosing(t *testing.T) {
	feed := func(chunks ...string) *tailReader {
		tr := newTailReader(strings.NewReader(strings.Join(chunks, "")), 8)
		buf := make([]byte, 2)
		for {
			_, err := tr.Read(buf)
			if err != nil {
				break
			}
		}
		return tr
	}
	if !feed("xxxxx--XX--\r\n").sawClosing("--XX--") {
		t.Fatal("closing with CRLF not detected")
	}
	if !feed("xxxxx--XX--").sawClosing("--XX--") {
		t.Fatal("closing without CRLF not detected")
	}
	if feed("xxxxx--XX\r\n").sawClosing("--XX--") {
		t.Fatal("bare boundary falsely detected as closing")
	}
	if feed("garbage").sawClosing("--XX--") {
		t.Fatal("random body falsely detected as closing")
	}
}
