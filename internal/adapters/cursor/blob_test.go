package cursor

import (
	"strings"
	"testing"

	"github.com/Rentheria/llm-agent-spend-manager/internal/tokenize"
)

// The blobs below are trimmed copies of what this machine's stores really hold
// (see blob.go for the census). The point of each case is a way the previous
// implementation — every byte of every blob, divided by 4 — got it wrong.
func TestModelVisibleText(t *testing.T) {
	tests := []struct {
		name string
		blob string
		want string
		why  string
	}{
		{
			name: "system y user guardan el contenido como string",
			blob: `{"role":"system","content":"You are an AI coding assistant."}`,
			want: "You are an AI coding assistant.",
			why:  "es el prompt tal cual salió al proveedor",
		},
		{
			name: "el turno del assistant suma texto y la llamada a herramienta",
			blob: `{"role":"assistant","content":[` +
				`{"type":"text","text":"Leo el archivo."},` +
				`{"type":"tool-call","toolCallId":"t1","toolName":"Read","args":{"path":"/x.go"}}]}`,
			want: `Leo el archivo.Read{"path":"/x.go"}`,
			why:  "el modelo emitió el nombre y los argumentos serializados, no una versión bonita",
		},
		{
			name: "el resultado de herramienta cuenta, venga en texto o en JSON",
			blob: `{"role":"tool","content":[` +
				`{"type":"tool-result","toolName":"Read","result":"linea 1\nlinea 2"},` +
				`{"type":"tool-result","toolName":"Grep","result":{"matches":2}}]}`,
			want: "linea 1\nlinea 2" + `{"matches":2}`,
			why:  "las dos formas regresaron al modelo",
		},
		{
			name: "el estado del editor en protobuf no cuenta",
			blob: "\n\x4e/home/user/projects/demo/internal/server/server_test.go2\x00\x01",
			want: "",
			why: "son 91.6% de los bytes en disco y NADA en disco dice que se hayan mandado " +
				"como prompt; contarlos fue el error que infló a Cursor ~11×",
		},
		{
			name: "un archivo cacheado sin rol no cuenta",
			blob: `{"name":"some-package","version":"0.5.1","dependencies":{"a":"1"}}`,
			want: "",
			why:  "su contenido ya viene dentro del tool-result que lo trajo; contarlo lo duplicaría",
		},
		{
			name: "JSON roto no revienta ni inventa",
			blob: `{"role":"user","content":`,
			want: "",
			why:  "un blob a medio escribir no puede convertirse en tokens",
		},
		{
			name: "vacío",
			blob: "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := modelVisibleText([]byte(tt.blob))
			if got != tt.want {
				t.Errorf("modelVisibleText() = %q, want %q\nporqué: %s", got, tt.want, tt.why)
			}
		})
	}
}

// providerOptions carries a SECOND copy of the same turn
// (`cursor.anthropicNativeContent`). Reading it would double every assistant
// message on disk, which is the kind of error that looks like real growth.
func TestModelVisibleText_IgnoresTheDuplicateCopyInProviderOptions(t *testing.T) {
	blob := `{"role":"assistant","content":[{"type":"text","text":"hola"}],` +
		`"providerOptions":{"cursor":{"anthropicNativeContent":"[{\"type\":\"text\",\"text\":\"hola\"}]"}}}`
	if got := modelVisibleText([]byte(blob)); got != "hola" {
		t.Errorf("modelVisibleText() = %q, want %q (providerOptions duplica el turno)", got, "hola")
	}
}

// The A floor has to stay a floor: a tokenizer that fails must cost the floor,
// never inflate it.
func TestTokenizeCount_EmptyTextIsZeroTokens(t *testing.T) {
	if got := tokenize.Count(""); got != 0 {
		t.Errorf("Count(\"\") = %d, want 0", got)
	}
}

// The whole reason for A2: the byte count and the token count are not
// proportional, so no single constant could have covered both prose and the
// dense JSON/code that dominates a coding agent's corpus.
func TestTokenizeCount_ProseAndCodeDoNotShareOneRatio(t *testing.T) {
	prose := strings.Repeat("El agente escribe una respuesta clara y corta. ", 40)
	code := strings.Repeat(`{"path":"/a/b.go","offset":12,"limit":3}`, 40)

	proseRatio := float64(len(prose)) / float64(tokenize.Count(prose))
	codeRatio := float64(len(code)) / float64(tokenize.Count(code))

	if proseRatio <= codeRatio {
		t.Errorf("bytes/token prosa %.2f, código %.2f: se esperaba que la prosa fuera MENOS densa; "+
			"si se emparejan, un solo ratio habría bastado y este ticket no tendría sentido",
			proseRatio, codeRatio)
	}
}
