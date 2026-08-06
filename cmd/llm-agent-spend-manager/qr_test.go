package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintQR_IncludesURLAndRenders(t *testing.T) {
	var buf bytes.Buffer
	if err := printQR(&buf, "http://192.168.1.42:4600"); err != nil {
		t.Fatalf("printQR error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "http://192.168.1.42:4600") {
		t.Errorf("QR output should include the URL as a text fallback:\n%s", out)
	}
	// The rendered code uses block glyphs; assert it produced a non-trivial grid.
	if !strings.Contains(out, "█") && !strings.Contains(out, "▀") && !strings.Contains(out, "▄") {
		t.Errorf("QR output should contain block glyphs, got:\n%q", out)
	}
	if len(out) < 200 {
		t.Errorf("QR output suspiciously short (%d bytes)", len(out))
	}
}
