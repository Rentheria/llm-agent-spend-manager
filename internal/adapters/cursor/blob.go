package cursor

import "encoding/json"

// A conversation's store.db holds one `blobs` table with two very different
// kinds of rows, and telling them apart is most of what this file is for
// (measured on this machine, 19 stores / 58.8 MB, 2026-07-30):
//
//   - **JSON message blobs** (8.4% of the bytes): `{"role":…,"content":…}` —
//     the actual turns, in the shape they were handed to the provider. This is
//     text we can PROVE reached the model.
//   - **Everything else** (91.6%): protobuf-framed editor state — workspace
//     URIs, file snapshots, checkpoints, opaque IDs. 81% of those bytes are
//     printable, so they look like text, but nothing on disk says which of them
//     were ever sent as prompt.
//
// The previous estimate counted every byte of both kinds and divided by 4. That
// is why the Cursor line read 108% of a monthly cap with a 14–203% range: the
// number was ~91% editor state weighed as if it were prompt. The measured floor
// dropped ~11× when only the provable text was counted (14.7M → 1.35M tokens).
//
// So the rule here is the house rule: count what can be shown, declare the rest.
// The protobuf blobs are not "assumed zero" — they are **undecidable from
// disk**, and the estimate's high end (`invisibleHeadroom`) is where the
// unmeasurable part is accounted for, in the open.

// message is one turn as Cursor stored it for the provider.
type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// providerOptions is deliberately NOT read: it carries a second copy of the
	// same turn (`anthropicNativeContent`), and counting it would double every
	// assistant message.
}

// contentPart is one piece of a structured turn (assistant / tool messages).
type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ToolName string          `json:"toolName"`
	Args     json.RawMessage `json:"args"`
	Result   json.RawMessage `json:"result"`
}

// modelVisibleText returns the text a blob proves went to the model, or "" for
// a blob that carries none.
func modelVisibleText(data []byte) string {
	// Cheap gate before paying for a JSON parse: the editor-state blobs are
	// protobuf and never start with '{'.
	if len(data) == 0 || data[0] != '{' {
		return ""
	}
	var m message
	if json.Unmarshal(data, &m) != nil || m.Role == "" {
		// Valid JSON with no role is a cached file (e.g. a package.json read by
		// a tool). Its content already shows up inside the tool result that
		// carried it, so counting it here would double it.
		return ""
	}

	// system and user turns store content as a plain string.
	var plain string
	if json.Unmarshal(m.Content, &plain) == nil {
		return plain
	}

	var parts []contentPart
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	var out []byte
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, p.Text...)
		case "tool-call":
			// The model was billed for the tool name and the arguments it
			// emitted, both serialized — not for a pretty rendering of them.
			out = append(out, p.ToolName...)
			out = append(out, p.Args...)
		case "tool-result":
			// A result is a string when the tool returned text, and a JSON
			// value when it returned structure; both went back to the model.
			var s string
			if json.Unmarshal(p.Result, &s) == nil {
				out = append(out, s...)
			} else {
				out = append(out, p.Result...)
			}
		}
	}
	return string(out)
}
