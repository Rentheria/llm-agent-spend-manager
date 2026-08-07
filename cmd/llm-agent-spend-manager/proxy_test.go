package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/enforce"
)

func TestRequireLoopback_AcceptsLoopbackForms(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:6399", "localhost:6379", "[::1]:6399", "127.0.0.53:6379"} {
		if err := requireLoopback(addr); err != nil {
			t.Errorf("requireLoopback(%q) = %v, quería nil", addr, err)
		}
	}
}

// The cap lives in Redis and the proxy forwards the user's upstream credentials
// untouched, so a non-loopback address is the exact shape of L-06. It must be
// refused at startup, not warned about.
func TestRequireLoopback_RejectsAnythingReachableFromTheNetwork(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:6399", "192.168.1.50:6379", "100.118.221.35:6379"} {
		err := requireLoopback(addr)
		if err == nil {
			t.Errorf("requireLoopback(%q) = nil, quería un error: expondría el tope a la red", addr)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("requireLoopback(%q) = %q, quería que dijera por qué (loopback)", addr, err)
		}
	}
}

func TestRequireLoopback_RejectsMalformedAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "", "no-es-una-ip:6399"} {
		if err := requireLoopback(addr); err == nil {
			t.Errorf("requireLoopback(%q) = nil, quería un error", addr)
		}
	}
}

// A typo in --unit must fail at startup: silently falling back to a default
// would cap on a different thing than the operator asked for, and nothing in the
// output would say so.
func TestBuildProxyConfig_RejectsUnknownUnitAndListsTheValidOnes(t *testing.T) {
	_, err := buildProxyConfig(4610, "dólares", "fleet")
	if err == nil {
		t.Fatal("buildProxyConfig con --unit desconocida = nil, quería un error")
	}
	for _, want := range []string{"tokens", "context-bytes", "requests"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("el error %q no menciona la opción válida %q", err, want)
		}
	}
}

// T118: the cap exists to stay under a ceiling measured in tokens, so the unit
// that counts the tokens the provider reports is the one you get without asking.
func TestBuildProxyConfig_CountsRealTokensByDefault(t *testing.T) {
	cfg, err := buildProxyConfig(4610, "tokens", "fleet")
	if err != nil {
		t.Fatalf("buildProxyConfig = %v, quería nil", err)
	}
	if !cfg.countsResponseUsage {
		t.Error("--unit tokens no activó la lectura del usage: el tope seguiría estimando")
	}
	if got := cfg.amount(httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("hola"))); got != 0 {
		t.Errorf("peso por adelantado = %d, quería 0: el real llega con la respuesta", got)
	}
}

func TestBuildProxyConfig_LeavesTheEstimateModesAlone(t *testing.T) {
	cfg, err := buildProxyConfig(4610, "context-bytes", "fleet")
	if err != nil {
		t.Fatalf("buildProxyConfig = %v, quería nil", err)
	}
	if cfg.countsResponseUsage {
		t.Error("context-bytes activó también el conteo por usage: contaría dos veces la misma llamada")
	}
}

// Bytes and tokens in the same bucket is meaningless arithmetic — and the
// upgrade that switched units would have found ~400M bytes sitting in the window
// and read them as tokens, 429ing the whole fleet on its first request.
func TestScopeKeyByUnit_SeparatesTheWindowsOfDifferentUnits(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	tokens, err := buildProxyConfig(4610, "tokens", "fleet")
	if err != nil {
		t.Fatalf("buildProxyConfig(tokens) = %v", err)
	}
	bytesUnit, err := buildProxyConfig(4611, "context-bytes", "fleet")
	if err != nil {
		t.Fatalf("buildProxyConfig(context-bytes) = %v", err)
	}
	if tokens.key(r) == bytesUnit.key(r) {
		t.Errorf("las dos unidades caen en la misma llave %q: sumarían bytes con tokens", tokens.key(r))
	}

	// Both lanes (4610 topa, 4611 solo cuenta) must keep sharing one bucket.
	interactive, err := buildProxyConfig(4611, "tokens", "fleet")
	if err != nil {
		t.Fatalf("buildProxyConfig(4611, tokens) = %v", err)
	}
	if tokens.key(r) != interactive.key(r) {
		t.Errorf("los dos carriles cayeron en llaves distintas (%q vs %q): el tope dejaría de ser combinado",
			tokens.key(r), interactive.key(r))
	}
}

func TestBuildProxyConfig_RejectsUnknownKey(t *testing.T) {
	if _, err := buildProxyConfig(4610, "context-bytes", "session"); err == nil {
		t.Fatal("buildProxyConfig con --key desconocida = nil, quería un error")
	}
}

// The listen address is not configurable by host on purpose: whatever the flags
// say, the proxy binds loopback (L-06).
func TestBuildProxyConfig_AlwaysBindsLoopback(t *testing.T) {
	cfg, err := buildProxyConfig(4610, "context-bytes", "fleet")
	if err != nil {
		t.Fatalf("buildProxyConfig = %v, quería nil", err)
	}
	if cfg.addr != "127.0.0.1:4610" {
		t.Errorf("addr = %q, quería 127.0.0.1:4610", cfg.addr)
	}
	if cfg.amount == nil || cfg.key == nil {
		t.Error("amount/key quedaron nil: el proxy correría sin unidad ni clave")
	}
}

// "cap 0" reads as "no budget left" when it actually means "never blocks". The
// banner is the only place an operator learns which of the two is running.
func TestProxyBanner_SaysCountingModeInWordsWhenTheCapIsZero(t *testing.T) {
	got := strings.Join(proxyBanner("127.0.0.1:4610", "https://api.anthropic.com",
		"en /tmp/cap.db", 0, 24*time.Hour, "context-bytes"), "\n")

	if !strings.Contains(got, "solo CUENTA") || !strings.Contains(got, "nunca bloquea") {
		t.Errorf("el banner con --cap 0 no dice que no bloquea:\n%s", got)
	}
	if strings.Contains(got, "TOPA") {
		t.Errorf("el banner con --cap 0 dice que topa:\n%s", got)
	}
}

func TestProxyBanner_SaysItIsEnforcingWhenTheCapIsSet(t *testing.T) {
	got := strings.Join(proxyBanner("127.0.0.1:4610", "https://api.anthropic.com",
		"en /tmp/cap.db", 250_000_000, 24*time.Hour, "context-bytes"), "\n")

	if !strings.Contains(got, "TOPA") {
		t.Errorf("el banner con tope no dice que topa:\n%s", got)
	}
	if !strings.Contains(got, "loopback") {
		t.Errorf("el banner no aclara que solo escucha en loopback:\n%s", got)
	}
}

// A raw byte count at fleet scale is unreadable, and reading it wrong is how an
// operator sets a cap 1000x off what they meant.
func TestFormatCap_ShowsBytesAlsoInMegabytes(t *testing.T) {
	got := formatCap(250_000_000, "context-bytes")
	if !strings.Contains(got, "250000000") || !strings.Contains(got, "MB") {
		t.Errorf("formatCap = %q, quería el conteo exacto y su equivalente en MB", got)
	}
}

func TestFormatCap_ShowsTokenCapsAlsoInMillions(t *testing.T) {
	got := formatCap(120_000_000, "tokens")
	if !strings.Contains(got, "120000000") || !strings.Contains(got, "120.0M") {
		t.Errorf("formatCap = %q, quería el conteo exacto y su equivalente en millones", got)
	}
}

// The operator who typed --unit context-bytes is the only one who can undo it,
// and the banner is where they look. Silence there is how a broken unit survives.
func TestProxyBanner_WarnsWhenTheUnitIsAnEstimate(t *testing.T) {
	estimate := strings.Join(proxyBanner("127.0.0.1:4610", "https://api.anthropic.com",
		"en /tmp/cap.db", 250_000_000, 5*time.Hour, "context-bytes"), "\n")
	if !strings.Contains(estimate, "ESTIMADO") || !strings.Contains(estimate, "--unit tokens") {
		t.Errorf("el banner no avisa que la unidad es un estimado ni a qué cambiarse:\n%s", estimate)
	}

	real := strings.Join(proxyBanner("127.0.0.1:4610", "https://api.anthropic.com",
		"en /tmp/cap.db", 120_000_000, 5*time.Hour, "tokens"), "\n")
	if strings.Contains(real, "ESTIMADO") {
		t.Errorf("el banner avisa de un estimado contando tokens reales:\n%s", real)
	}
}

func TestFormatCap_LeavesNonByteUnitsAlone(t *testing.T) {
	if got := formatCap(500, "requests"); got != "500" {
		t.Errorf("formatCap(500, requests) = %q, quería 500 sin conversión a MB", got)
	}
}

// A proxy that runs as a service restarts on every upgrade, crash and reboot.
// Defaulting the tally to RAM would hand the fleet a fresh budget each time, so
// the default has to be the file — with no flags at all.
func TestBuildCounter_DefaultsToTheDurableFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	counter, tally, err := buildCounter("", "")
	if err != nil {
		t.Fatalf("buildCounter = %v, quería nil", err)
	}
	if closer, ok := counter.(io.Closer); ok {
		defer closer.Close()
	}
	if _, ok := counter.(*enforce.SQLiteCounter); !ok {
		t.Errorf("contador por default = %T, quería *enforce.SQLiteCounter", counter)
	}
	if !strings.Contains(tally, "sobrevive") {
		t.Errorf("el banner dice %q, quería que aclarara que sobrevive al reinicio", tally)
	}
}

func TestBuildCounter_MemoryIsOptInAndSaysWhatItCosts(t *testing.T) {
	counter, tally, err := buildCounter("", "memory")
	if err != nil {
		t.Fatalf("buildCounter = %v, quería nil", err)
	}
	if _, ok := counter.(*enforce.MemoryCounter); !ok {
		t.Errorf("contador = %T, quería *enforce.MemoryCounter", counter)
	}
	if !strings.Contains(tally, "se pierde al reiniciar") {
		t.Errorf("el banner dice %q, quería que avisara que se pierde al reiniciar", tally)
	}
}

// The password may never ride in argv, where any local user reads it off `ps`
// (L-06). Refusing to start is the only way that rule holds.
func TestBuildCounter_RefusesRedisWithoutThePasswordEnvVar(t *testing.T) {
	t.Setenv(redisPasswordEnvVar, "")

	_, _, err := buildCounter("127.0.0.1:6399", "")
	if err == nil {
		t.Fatal("buildCounter con --redis y sin contraseña = nil, quería un error")
	}
	if !strings.Contains(err.Error(), redisPasswordEnvVar) {
		t.Errorf("el error %q no nombra la variable que hace falta", err)
	}
}

func TestBuildCounter_RefusesRedisOffLoopback(t *testing.T) {
	t.Setenv(redisPasswordEnvVar, "irrelevante")

	if _, _, err := buildCounter("192.168.1.50:6379", ""); err == nil {
		t.Fatal("buildCounter con un Redis en la LAN = nil, quería un error")
	}
}

// TestReadableWindow_OnlyWhereThePhaseCanActuallyBeReconstructed: the note the
// 429 carries is built from this fleet's own transcripts, which describe the
// Anthropic account and nothing else. Attaching it to another upstream would
// state a reset that has no relation to the quota being enforced.
func TestReadableWindow_OnlyWhereThePhaseCanActuallyBeReconstructed(t *testing.T) {
	cases := map[string]bool{
		defaultProxyTarget:                true,
		"https://api.anthropic.com/v1":    true,
		"https://api.anthropic.com:8443/": true,
		"https://anthropic.com":           true,
		"https://api.openai.com":          false,
		// Suffix matching on the raw string would take this for Anthropic's.
		"https://anthropic.com.attacker.example": false,
		"https://notanthropic.com":               false,
		"":                                       false,
		"://roto":                                false,
	}
	for target, want := range cases {
		if got := readableWindow(target); got != want {
			t.Errorf("readableWindow(%q) = %v, want %v", target, got, want)
		}
	}
}

// TestResetNoteFor_LeavesOtherProvidersWithoutANote is the wiring side of the
// same rule: no note at all beats an invented one.
func TestResetNoteFor_LeavesOtherProvidersWithoutANote(t *testing.T) {
	note, err := resetNoteFor("https://api.openai.com")
	if err != nil {
		t.Fatalf("resetNoteFor error: %v", err)
	}
	if note != nil {
		t.Errorf("nota = %q, want ninguna para un proveedor cuya ventana no leemos", note())
	}
}
