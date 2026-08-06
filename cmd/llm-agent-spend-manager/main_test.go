package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_Version(t *testing.T) {
	var buf bytes.Buffer

	code := run([]string{"--version"}, &buf)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), version) {
		t.Fatalf("output = %q, want it to contain %q", buf.String(), version)
	}
}

func TestRun_NoArgsPrintsUsage(t *testing.T) {
	var buf bytes.Buffer

	code := run(nil, &buf)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), "Usage:") {
		t.Fatalf("output = %q, want usage text", buf.String())
	}
}

func TestRun_StatusCommand(t *testing.T) {
	var buf bytes.Buffer

	code := run([]string{"status"}, &buf)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	var buf bytes.Buffer

	code := run([]string{"bogus"}, &buf)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(buf.String(), "unknown command") {
		t.Fatalf("output = %q, want it to mention the unknown command", buf.String())
	}
}
