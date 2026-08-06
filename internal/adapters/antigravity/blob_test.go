package antigravity

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// --- protobuf builders ------------------------------------------------------
//
// The fixtures below are hand-encoded because Antigravity publishes no .proto:
// there is no generated marshaller to borrow, and the decoder under test reads
// the wire format directly. Encoding by hand here is what makes the fixture and
// the decoder independent — a renumbered field breaks the test instead of
// silently agreeing with it.

// field encodes one length-delimited (wire type 2) field: a string, or a nested
// message already encoded by another call to field.
func field(number int, payload []byte) []byte {
	key := binary.AppendUvarint(nil, uint64(number)<<3|wireBytes)
	return concat(key, binary.AppendUvarint(nil, uint64(len(payload))), payload)
}

// varintField encodes one varint (wire type 0) field, e.g. a message's role.
func varintField(number int, value uint64) []byte {
	key := binary.AppendUvarint(nil, uint64(number)<<3|wireVarint)
	return concat(key, binary.AppendUvarint(nil, value))
}

func concat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

// genMetadataBlob builds the shape of a real payload row: a request carrying the
// model id, the assembled system prompt, and one user message.
func genMetadataBlob(model, systemPrompt, userText string) []byte {
	request := concat(
		field(fieldModelID, []byte(model)),
		field(fieldMessage, concat(
			varintField(2, 1), // role: user
			field(fieldMessageText, []byte(userText)),
		)),
	)
	if systemPrompt != "" {
		request = concat(field(fieldSystemPrompt, []byte(systemPrompt)), request)
	}
	return field(fieldRequest, request)
}

// --- the decoder ------------------------------------------------------------

// Each case is a way the floor could go wrong: text the store proves was sent
// and must be counted, or bytes that were NOT proven sent and must not be.
func TestModelVisibleText(t *testing.T) {
	tests := []struct {
		name string
		blob []byte
		want string
		why  string
	}{
		{
			name: "the system prompt and the message history both count",
			blob: genMetadataBlob("claude-opus-4-6", "You are an agent.", "fix the test"),
			want: "You are an agent.fix the test",
			why:  "both went out in the same request",
		},
		{
			name: "a stub row yields nothing",
			blob: field(fieldRequest, field(fieldModelID, []byte("gemini-pro-default"))),
			want: "",
			why: "most gen_metadata rows are ~1 KB stubs with no history; returning \"\" is " +
				"how the caller picks out the one row that carries the payload",
		},
		{
			name: "extended thinking counts",
			blob: field(fieldRequest, field(fieldMessage, concat(
				field(fieldMessageText, []byte("done")),
				field(fieldMessageThinking, []byte("let me check the estimator")),
			))),
			want: "donelet me check the estimator",
			why:  "thinking is billed as ordinary output tokens, so it belongs in the floor",
		},
		{
			name: "a tool call counts its name and its arguments",
			blob: field(fieldRequest, field(fieldMessage, field(fieldMessageToolCall, concat(
				field(fieldToolCallName, []byte("read_file")),
				field(fieldToolCallArguments, []byte(`{"path":"/x.go"}`)),
			)))),
			want: `read_file{"path":"/x.go"}`,
			why:  "the model emitted the serialized call, not a pretty version of it",
		},
		{
			name: "tool definitions count once even though the blob stores the name twice",
			blob: field(fieldRequest, concat(
				field(fieldToolDefinition, concat(
					field(fieldToolName, []byte("grep")),
					field(fieldToolDescription, []byte("search files")),
					field(fieldToolSchema, []byte(`{"pattern":"string"}`)),
					field(7, []byte("grep")), // the same name again
				)),
				field(fieldMessage, field(fieldMessageText, []byte("go"))),
			)),
			want: `grepsearch files{"pattern":"string"}go`,
			why:  "field 7 repeated the name in 189/189 occurrences; counting it would inflate the floor",
		},
		{
			name: "the second copy of the system prompt does not count",
			blob: field(fieldRequest, concat(
				field(fieldSystemPrompt, []byte("identity: you are an agent")),
				field(16, field(1, []byte("identity: you are an agent"))), // named chunks, same text
				field(fieldMessage, field(fieldMessageText, []byte("hi"))),
			)),
			want: "identity: you are an agenthi",
			why:  "every chunk of field 16 was verified a substring of the system prompt in 12/12 conversations",
		},
		{
			name: "an opaque signature does not count",
			blob: field(fieldRequest, field(fieldMessage, concat(
				field(fieldMessageText, []byte("ok")),
				field(20, []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}),
			))),
			want: "ok",
			why: "a Vertex thought signature is not text; disk cannot prove it was sent as tokens, " +
				"so it is paid for in the range's headroom instead",
		},
		{
			name: "a truncated blob yields nothing rather than guessing",
			blob: []byte{0x0a, 0xff},
			want: "",
			why:  "a declared length past the end of the buffer means this is not the format we mapped",
		},
		{
			name: "a blob with no request field yields nothing",
			blob: []byte("not protobuf at all"),
			want: "",
			why:  "the format can change on any release; the caller must fall back, not report a number",
		},
		{
			name: "empty",
			blob: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelVisibleText(tt.blob); got != tt.want {
				t.Errorf("modelVisibleText() = %q, want %q\nwhy: %s", got, tt.want, tt.why)
			}
		})
	}
}

func TestBlobModelID(t *testing.T) {
	tests := []struct {
		name string
		blob []byte
		want string
	}{
		{
			name: "reads the model the row was sent to",
			blob: genMetadataBlob("claude-opus-4-6-thinking", "s", "u"),
			want: "claude-opus-4-6-thinking",
		},
		{
			name: "a row that names no model yields empty, not a guess",
			blob: field(fieldRequest, field(fieldMessage, field(fieldMessageText, []byte("hi")))),
			want: "",
		},
		{
			name: "an undecodable blob yields empty",
			blob: []byte{0x0a, 0xff},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blobModelID(tt.blob); got != tt.want {
				t.Errorf("blobModelID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The decoder walks attacker-shaped input (an undocumented format on disk), so
// it must fail soft on anything malformed rather than panic or loop.
func TestParseWireFields_RejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{"length past the end of the buffer", []byte{0x0a, 0xff}},
		{"field number zero", []byte{0x00, 0x00}},
		{"deprecated start-group wire type", []byte{0x0b}},
		{"truncated fixed64", []byte{0x09, 0x01, 0x02}},
		{"truncated fixed32", []byte{0x0d, 0x01}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := parseWireFields(tt.payload); ok {
				t.Errorf("parseWireFields(%v) accepted malformed input", tt.payload)
			}
		})
	}
}

// Without a .proto there is no type tag on the wire, so this heuristic is the
// only thing keeping binary payloads out of the token floor.
func TestReadableText(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{"plain text", []byte("hola"), "hola"},
		{"text with newlines and tabs", []byte("a\n\tb"), "a\n\tb"},
		{"accented UTF-8 survives", []byte("configuración"), "configuración"},
		{"invalid UTF-8 is not text", []byte{0xff, 0xfe, 0xfd}, ""},
		{"control bytes are not text", []byte{0x00, 0x01, 0x02, 0x03}, ""},
		{"empty", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readableText(tt.payload); got != tt.want {
				t.Errorf("readableText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The floor counts real text, so a mostly-text payload with the odd stray
// control byte inside tool output must survive the ratio check.
func TestReadableText_ToleratesStrayControlBytesInsideRealText(t *testing.T) {
	payload := append([]byte(strings.Repeat("real tool output ", 10)), 0x01)
	if got := readableText(payload); got == "" {
		t.Error("readableText() = \"\", want the text kept: one control byte must not discard real output")
	}
}
