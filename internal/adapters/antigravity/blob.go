package antigravity

import (
	"encoding/binary"
	"strings"
	"unicode/utf8"
)

// Decoder for Antigravity's gen_metadata.data blob — the Camino A path.
//
// Antigravity publishes no .proto and no schema documentation, so the field
// numbers below were recovered by walking the raw protobuf wire format (field
// number + wire type) over every conversation on this machine. They are
// OBSERVATIONS, not a contract: Google can renumber them in any release without
// telling anyone. Every function here therefore fails soft — an unparseable or
// renumbered blob yields "" and the caller falls back to the Camino B count,
// never to a zero and never to a guess (docs/calibracion.md §Antigravity).
//
// What the recon established, and the evidence for it, is in docs/calibracion.md.
// The two facts that shape this file:
//
//   - Only ONE row of gen_metadata carries the conversation payload (the last
//     one); the other rows are ~1 KB stubs holding just a model id and request
//     id. So the floor is read from that single row, which holds the full final
//     message history.
//   - The blob stores several things twice. Counting both copies would inflate
//     the floor with text that was sent once, so each duplicate pair below is
//     read from exactly one side, and which side is named at the field.
const (
	// fieldRequest is the request actually sent to the model — everything this
	// file reads lives under it.
	fieldRequest = 1

	// Inside the request.
	fieldSystemPrompt   = 1  // assembled system prompt, one string
	fieldMessage        = 2  // repeated: the conversation's message history
	fieldToolDefinition = 8  // repeated: the tool schemas offered to the model
	fieldModelID        = 19 // e.g. "claude-opus-4-6-thinking", "gemini-3-flash-a"

	// Inside a message. Field 2 is a varint role (1 user, 2 assistant, 4 tool
	// result) that this file decodes but does not branch on: every role's text
	// was sent to the model, so all of it belongs in the floor.
	fieldMessageText     = 3  // the message's text, whatever the role
	fieldMessageToolCall = 6  // repeated: tool invocations by the assistant
	fieldMessageThinking = 11 // extended-thinking text

	// Inside a tool call and a tool definition.
	fieldToolCallName      = 2
	fieldToolCallArguments = 3
	fieldToolName          = 1
	fieldToolDescription   = 2
	fieldToolSchema        = 3
)

// Fields deliberately NOT read, because reading them would double-count text
// that was sent once:
//
//   - request field 16: the same system prompt again, split into named chunks
//     ("identity", "user_information", "skills", …). Every chunk was verified to
//     be a substring of fieldSystemPrompt in 12/12 conversations.
//   - tool-call field 9 and tool-definition field 7: the tool's name a second
//     time (identical in 154/154 and 189/189 occurrences respectively).
//
// Fields deliberately NOT counted, because the store cannot prove they were sent
// as text — they are declared undecidable and paid for in the range's headroom
// rather than counted as zero (the house rule, docs/calibracion.md):
//
//   - message field 20 and tool-call field 7: opaque signed/encrypted blobs
//     (a constant binary prefix, high entropy) — Vertex thought signatures.
//   - message fields 23 and 25: structured result protos whose text was verified
//     NOT to appear in fieldMessageText (0/21 and 0/7), so their status as sent
//     content is genuinely unknown.

// printableTextRatio is the share of runes that must be printable for a
// length-delimited field to be treated as text rather than as a packed or binary
// payload. Protobuf gives no type tag on the wire, so this is the only available
// discriminator; 0.90 tolerates the odd control byte inside real tool output
// without admitting binary signatures.
const printableTextRatio = 0.90

// wireField is one decoded protobuf field: its number, its wire type, and
// whichever of the two payload shapes that wire type carries.
type wireField struct {
	number   int
	wireType int
	value    uint64 // varint / fixed32 / fixed64
	bytes    []byte // length-delimited
}

// Protobuf wire types (protobuf encoding spec). Types 3 and 4 (the deprecated
// start/end group) are not emitted by any observed blob and are rejected.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

// parseWireFields decodes one protobuf message into its top-level fields. ok is
// false the moment anything doesn't line up — a truncated length, an unknown
// wire type, a field number of zero. Callers treat that as "not a message" and
// move on, which is also how nested parsing tells a submessage apart from a
// string that merely looks like one.
func parseWireFields(payload []byte) (fields []wireField, ok bool) {
	for offset := 0; offset < len(payload); {
		key, keyLen := binary.Uvarint(payload[offset:])
		if keyLen <= 0 {
			return nil, false
		}
		offset += keyLen

		field := wireField{number: int(key >> 3), wireType: int(key & 7)}
		if field.number == 0 {
			return nil, false
		}

		read, advance := payloadReaders[field.wireType], 0
		if read == nil {
			return nil, false
		}
		if field.value, field.bytes, advance, ok = read(payload[offset:]); !ok {
			return nil, false
		}
		offset += advance
		fields = append(fields, field)
	}
	return fields, true
}

// payloadReader decodes one field's payload, returning how many bytes it spans.
type payloadReader func(rest []byte) (value uint64, bytes []byte, advance int, ok bool)

var payloadReaders = map[int]payloadReader{
	wireVarint: func(rest []byte) (uint64, []byte, int, bool) {
		value, n := binary.Uvarint(rest)
		return value, nil, n, n > 0
	},
	wireFixed64: func(rest []byte) (uint64, []byte, int, bool) {
		if len(rest) < 8 {
			return 0, nil, 0, false
		}
		return binary.LittleEndian.Uint64(rest), nil, 8, true
	},
	wireFixed32: func(rest []byte) (uint64, []byte, int, bool) {
		if len(rest) < 4 {
			return 0, nil, 0, false
		}
		return uint64(binary.LittleEndian.Uint32(rest)), nil, 4, true
	},
	wireBytes: func(rest []byte) (uint64, []byte, int, bool) {
		length, n := binary.Uvarint(rest)
		if n <= 0 || uint64(n)+length > uint64(len(rest)) {
			return 0, nil, 0, false
		}
		return 0, rest[n : uint64(n)+length], n + int(length), true
	},
}

// submessages returns every length-delimited field with the given number, in
// wire order (protobuf repeats a field number rather than nesting a list).
func submessages(payload []byte, number int) [][]byte {
	fields, ok := parseWireFields(payload)
	if !ok {
		return nil
	}
	var found [][]byte
	for _, field := range fields {
		if field.number == number && field.wireType == wireBytes {
			found = append(found, field.bytes)
		}
	}
	return found
}

// firstSubmessage returns the first length-delimited field with the given
// number, or nil when absent.
func firstSubmessage(payload []byte, number int) []byte {
	found := submessages(payload, number)
	if len(found) == 0 {
		return nil
	}
	return found[0]
}

// readableText returns the payload as a string when it is plausibly text, and ""
// when it is binary — a signature, a packed field, a nested message. Without a
// .proto there is no type information on the wire, so this heuristic is what
// keeps opaque bytes out of the token floor.
func readableText(payload []byte) string {
	if !utf8.Valid(payload) {
		return ""
	}
	text := string(payload)
	printable, total := 0, 0
	for _, r := range text {
		total++
		if r == '\n' || r == '\t' || r == '\r' || (r >= ' ' && r != utf8.RuneError) {
			printable++
		}
	}
	if total == 0 || float64(printable)/float64(total) < printableTextRatio {
		return ""
	}
	return text
}

// blobModelID returns the model a gen_metadata row was sent to, or "" when the
// row doesn't name one. Callers vote across rows rather than trusting a single
// one: the row carrying the conversation payload does not always carry the model
// id (111 of 112 rows had it; one conversation's payload row did not).
func blobModelID(blob []byte) string {
	request := firstSubmessage(blob, fieldRequest)
	if request == nil {
		return ""
	}
	return readableText(firstSubmessage(request, fieldModelID))
}

// modelVisibleText returns the text a gen_metadata row proves was sent to the
// model: the system prompt, the tool definitions offered with it, and every
// message in the history. It returns "" for the stub rows that carry no message
// history, so the caller can pick out the one row that holds the payload.
//
// Mirrors cursor's modelVisibleText: the point is a FLOOR — text the store
// proves went to the model, counted once — not a reconstruction of the
// conversation. Context re-sent on every turn is deliberately counted once here
// and accounted for by the range's headroom (estimate.go), exactly as in Cursor.
func modelVisibleText(blob []byte) string {
	request := firstSubmessage(blob, fieldRequest)
	if request == nil {
		return ""
	}
	messages := submessages(request, fieldMessage)
	if len(messages) == 0 {
		return "" // a stub row: model id and request id only
	}

	var sent strings.Builder
	sent.WriteString(readableText(firstSubmessage(request, fieldSystemPrompt)))
	writeToolDefinitions(&sent, request)
	for _, message := range messages {
		writeMessage(&sent, message)
	}
	return sent.String()
}

// writeToolDefinitions appends the tool schemas offered to the model. Their name
// is read once even though the blob stores it twice (see the constants above).
func writeToolDefinitions(sent *strings.Builder, request []byte) {
	for _, tool := range submessages(request, fieldToolDefinition) {
		for _, number := range []int{fieldToolName, fieldToolDescription, fieldToolSchema} {
			sent.WriteString(readableText(firstSubmessage(tool, number)))
		}
	}
}

// writeMessage appends one message's text, its extended thinking, and any tool
// calls it made. The role is decoded but not branched on: every role's text was
// sent to the model, so all of it belongs in the floor.
func writeMessage(sent *strings.Builder, message []byte) {
	for _, text := range submessages(message, fieldMessageText) {
		sent.WriteString(readableText(text))
	}
	for _, thinking := range submessages(message, fieldMessageThinking) {
		sent.WriteString(readableText(thinking))
	}
	for _, call := range submessages(message, fieldMessageToolCall) {
		sent.WriteString(readableText(firstSubmessage(call, fieldToolCallName)))
		sent.WriteString(readableText(firstSubmessage(call, fieldToolCallArguments)))
	}
}
