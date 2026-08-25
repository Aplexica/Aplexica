package proto

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestFrameReaderAndWriterRejectOversize(t *testing.T) {
	big := bytes.Repeat([]byte{'x'}, MaxJSONRPCFrameBytes+1)
	var out bytes.Buffer
	if err := NewFrameWriter(&out).Write(big); err == nil {
		t.Fatal("oversized outbound frame accepted")
	}
	big = append(big, '\n')
	if _, err := NewFrameReader(bytes.NewReader(big)).Read(); err == nil {
		t.Fatal("oversized inbound frame accepted")
	}
}

func TestWriteMessageAppendsNewline(t *testing.T) {
	var buf bytes.Buffer
	w := NewFrameWriter(&buf)
	if err := w.Write([]byte(`{"jsonrpc":"2.0"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := buf.String()
	if got != `{"jsonrpc":"2.0"}`+"\n" {
		t.Errorf("got %q, want object + newline", got)
	}
}

func TestReadMessageSingleFrame(t *testing.T) {
	r := NewFrameReader(strings.NewReader(`{"a":1}` + "\n"))
	frame, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(frame) != `{"a":1}` {
		t.Errorf("got %q", frame)
	}
}

func TestReadMessageMultipleFrames(t *testing.T) {
	r := NewFrameReader(strings.NewReader(`{"a":1}` + "\n" + `{"b":2}` + "\n"))
	got := []string{}
	for {
		frame, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got = append(got, string(frame))
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Errorf("got %v", got)
	}
}

func TestReadMessageOver64MiBLimitRejected(t *testing.T) {
	payload := strings.Repeat("x", 65*1024*1024)
	r := NewFrameReader(strings.NewReader(payload + "\n"))
	if _, err := r.Read(); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestReadMessageEOF(t *testing.T) {
	r := NewFrameReader(strings.NewReader(""))
	_, err := r.Read()
	if err != io.EOF {
		t.Errorf("got %v, want io.EOF", err)
	}
}

func TestReadMessageEOFAfterPartialLine(t *testing.T) {
	r := NewFrameReader(strings.NewReader(`{"a":1}`))
	frame, err := r.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(frame) != `{"a":1}` {
		t.Errorf("partial line: got %q", frame)
	}
	_, err = r.Read()
	if err != io.EOF {
		t.Errorf("after partial: got %v, want io.EOF", err)
	}
}
