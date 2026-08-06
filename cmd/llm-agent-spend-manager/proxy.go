package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Rentheria/llm-agent-spend-manager/internal/enforce"
	"github.com/Rentheria/llm-agent-spend-manager/internal/proxy"
)

const (
	defaultProxyPort = 4610
	// defaultProxyTarget is Anthropic because that is the only provider every
	// agent on this machine shares (Claude Code directly, OpenClaw through the
	// claude-cli backend). Point --target elsewhere for another provider.
	defaultProxyTarget = "https://api.anthropic.com"
	// defaultRedisAddr deliberately is NOT the usual 6379: that instance is
	// shared with other projects on this machine, and this cap requires its
	// Redis to carry a password. A dedicated instance keeps us from forcing
	// requirepass on someone else's database.
	defaultRedisAddr = "127.0.0.1:6399"
	defaultCapWindow = 24 * time.Hour
	// redisPasswordEnvVar carries the Redis password so it never appears in the
	// process argv, where any local user could read it off `ps` (L-06).
	redisPasswordEnvVar = "SPEND_REDIS_PASSWORD"
	// memoryStateValue is the --state escape hatch for a throwaway proxy that
	// should touch no disk at all.
	memoryStateValue = "memory"
)

// unitMode is how a --unit choice charges the counter: a weight known when the
// request goes out, the real usage the provider reports back, or both.
type unitMode struct {
	amount proxy.AmountFunc
	// countsResponseUsage adds the provider's reported tokens after the call.
	countsResponseUsage bool
}

// unitModes maps the --unit flag to how each request is charged.
//
// tokens is the default (T118, 2026-08-05). The cap exists to keep the fleet
// under a plan ceiling measured in TOKENS, and every other unit here is a proxy
// variable for that one — context-bytes was the closest available before the
// response could be read, and it turned out to be off by 7x under prompt caching
// (see proxy.ContextBytesAmount). Counting what Anthropic actually reports
// removes the translation, and with it the whole class of error.
var unitModes = map[string]unitMode{
	"tokens":        {amount: proxy.NoUpfrontAmount, countsResponseUsage: true},
	"context-bytes": {amount: proxy.ContextBytesAmount},
	"requests":      {amount: proxy.OnePerRequest},
}

// keyFuncs maps the --key flag to how the counter key is chosen. fleet is the
// default because a shared key is what makes the cap combined across agents
// that do not know about each other.
var keyFuncs = map[string]proxy.KeyFunc{
	"fleet": proxy.DefaultKey,
	"agent": proxy.AgentHeaderKey,
}

// cmdProxy runs the enforcement proxy (architecture.md §3.2). It always binds
// loopback: the proxy forwards the user's upstream credentials untouched, so
// binding the LAN would let any peer spend with those keys (L-06). Crossing the
// network would first require the proxy to authenticate its own callers.
//
// The cap defaults to 0 — count, never block. That is the safe way to wire a
// live fleet: confirm every agent's traffic actually flows through the proxy
// before a number can cut anyone off mid-task.
func cmdProxy(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(out)
	port := fs.Int("port", defaultProxyPort, "TCP port to listen on (loopback only)")
	target := fs.String("target", defaultProxyTarget, "upstream provider base URL")
	state := fs.String("state", "", "file the cap window is kept in; empty uses the default path, \"memory\" keeps it in RAM (lost on restart)")
	redisAddr := fs.String("redis", "", "share the cap across MACHINES via Redis (loopback address, e.g. "+defaultRedisAddr+"); unnecessary on a single machine")
	capLimit := fs.Int64("cap", 0, "combined cap per window in the --unit chosen; 0 counts without ever blocking")
	window := fs.Duration("window", defaultCapWindow, "rolling window the cap applies over")
	unit := fs.String("unit", "tokens", "what each request adds to the counter: tokens (real usage reported by the provider)|context-bytes (estimate, off by ~7x under prompt caching)|requests")
	key := fs.String("key", "fleet", "how the counter key is chosen: fleet (combined)|agent (per X-Agent header)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := buildProxyConfig(*port, *unit, *key)
	if err != nil {
		fmt.Fprintln(out, "configuración inválida:", err)
		return 2
	}

	counter, tally, err := buildCounter(*redisAddr, *state)
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	if closer, ok := counter.(io.Closer); ok {
		defer closer.Close()
	}

	limiter := enforce.New(counter, *window, *capLimit)
	opts := []proxy.Option{proxy.WithAmountFunc(cfg.amount), proxy.WithKeyFunc(cfg.key)}
	if cfg.countsResponseUsage {
		opts = append(opts, proxy.WithUsageCounting())
	}
	px, err := proxy.New(*target, limiter, opts...)
	if err != nil {
		fmt.Fprintln(out, "no pude construir el proxy:", err)
		return 1
	}

	for _, line := range proxyBanner(cfg.addr, *target, tally, *capLimit, *window, *unit) {
		fmt.Fprintln(out, line)
	}

	if err := http.ListenAndServe(cfg.addr, px); err != nil {
		fmt.Fprintln(out, "proxy detenido:", err)
		return 1
	}
	return 0
}

// buildCounter picks where the cap's tally lives, and returns a description of
// the choice for the banner.
//
// The default is a file: a proxy meant to run as a service restarts on every
// upgrade, crash and reboot, and a cap that forgets on restart is a cap the
// fleet can walk around. SQLite costs nothing to install (the driver is already
// linked in for the adapters) and its write lock is held across processes, so
// two proxies on one machine still share one cap.
//
// Redis is left for the single case neither covers — a cap shared across
// machines — and then L-06 applies: loopback only, and a password that never
// rides in argv.
func buildCounter(redisAddr, state string) (enforce.Counter, string, error) {
	if redisAddr != "" {
		if err := requireLoopback(redisAddr); err != nil {
			return nil, "", fmt.Errorf("--redis %q: %w", redisAddr, err)
		}
		password := os.Getenv(redisPasswordEnvVar)
		if password == "" {
			return nil, "", fmt.Errorf("--redis pide %s: el Redis del tope necesita contraseña aunque sea local (L-06).\n"+
				"Exporta la contraseña de tu instancia, o quita --redis para llevar la cuenta en un archivo local", redisPasswordEnvVar)
		}
		counter := enforce.NewRedisCounter(redis.NewClient(&redis.Options{Addr: redisAddr, Password: password}))
		return counter, fmt.Sprintf("en Redis %s (compartido entre máquinas)", redisAddr), nil
	}

	if state == memoryStateValue {
		return enforce.NewMemoryCounter(), "en memoria de este proceso (se pierde al reiniciar)", nil
	}

	path := state
	if path == "" {
		def, err := enforce.DefaultStatePath()
		if err != nil {
			return nil, "", err
		}
		path = def
	}
	counter, err := enforce.NewSQLiteCounter(path)
	if err != nil {
		return nil, "", err
	}
	return counter, fmt.Sprintf("en %s (sobrevive reinicios; sin dependencias)", path), nil
}

// proxyConfig holds the validated pieces cmdProxy needs after flag parsing.
type proxyConfig struct {
	addr                string
	amount              proxy.AmountFunc
	key                 proxy.KeyFunc
	countsResponseUsage bool
}

// buildProxyConfig validates the flags at the boundary and fails fast, so a typo
// in --unit surfaces at startup instead of silently capping on the wrong thing.
func buildProxyConfig(port int, unit, key string) (proxyConfig, error) {
	mode, ok := unitModes[unit]
	if !ok {
		return proxyConfig{}, fmt.Errorf("--unit %q: usa %s", unit, strings.Join(sortedKeys(unitModes), " o "))
	}
	keyFn, ok := keyFuncs[key]
	if !ok {
		return proxyConfig{}, fmt.Errorf("--key %q: usa %s", key, strings.Join(sortedKeys(keyFuncs), " o "))
	}
	return proxyConfig{
		addr:                net.JoinHostPort("127.0.0.1", fmt.Sprint(port)),
		amount:              mode.amount,
		key:                 scopeKeyByUnit(unit, keyFn),
		countsResponseUsage: mode.countsResponseUsage,
	}, nil
}

// scopeKeyByUnit puts the unit in the counter key, so changing --unit starts a
// clean window instead of inheriting a number that used to mean something else.
//
// This is not cosmetic. The window in cap.db held ~400 million when the unit was
// context-bytes; read as tokens against a token cap in the hundred millions, the
// fleet would have been over its cap at the first request after the upgrade —
// every agent 429ing on a counter measured in the wrong unit. Two units in one
// bucket is meaningless arithmetic; the key makes that impossible rather than
// leaving it to whoever does the deploy. Same rule --window already carries
// (changing it discards the accumulated count), now enforced.
//
// Both lanes (4610 enforcing, 4611 counting) derive the key the same way, so
// they keep sharing one bucket as long as they share --unit and --key.
func scopeKeyByUnit(unit string, fn proxy.KeyFunc) proxy.KeyFunc {
	return func(r *http.Request) string { return unit + ":" + fn(r) }
}

// requireLoopback rejects any address that is not on the loopback interface.
// The cap lives in Redis: a Redis reachable from the network is a cap anyone on
// that network can reset to zero (L-06).
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("no es una dirección host:puerto válida: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%q no es una IP; solo se acepta loopback", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("%q no es loopback; el tope y sus credenciales nunca cruzan la red (L-06)", host)
	}
	return nil
}

// proxyBanner builds the startup lines. It states the enforcement mode in words
// because "cap 0" reads as "no budget" when it actually means "never blocks",
// and it names where the tally lives because that decides what a restart costs.
func proxyBanner(addr, target, tally string, capLimit int64, window time.Duration, unit string) []string {
	mode := fmt.Sprintf("cuenta y TOPA en %s por %s rodante (%s)", formatCap(capLimit, unit), window, unit)
	if capLimit <= 0 {
		mode = fmt.Sprintf("solo CUENTA, nunca bloquea (--cap 0, unidad %s)", unit)
	}
	lines := []string{
		"llm-agent-spend-manager — proxy de tope combinado (Fase 3)",
		fmt.Sprintf("Escucha:    http://%s   (solo loopback)", addr),
		fmt.Sprintf("Reenvía a:  %s", target),
		fmt.Sprintf("Contador:   %s", tally),
		fmt.Sprintf("Modo:       %s", mode),
	}
	if unit != tokenUnit {
		// The operator who typed this flag is the only one who can undo it, and the
		// banner is the one place they look. It says what it costs, not just that
		// something changed.
		lines = append(lines, fmt.Sprintf(
			"OJO:        %q es un ESTIMADO del gasto real; medido contra el `usage` de Anthropic se fue ~7x (T118). Usa --unit %s.", unit, tokenUnit))
	}
	return lines
}

// tokenUnit is the unit that counts what the provider reports instead of
// estimating it.
const tokenUnit = "tokens"

// formatCap spells a cap at fleet scale in something readable next to its exact
// value: reading 250000000 wrong is how an operator sets a cap 1000x off what
// they meant.
func formatCap(capLimit int64, unit string) string {
	switch unit {
	case "context-bytes":
		return fmt.Sprintf("%d bytes (~%.0f MB)", capLimit, float64(capLimit)/(1024*1024))
	case tokenUnit:
		return fmt.Sprintf("%d tokens (~%.1fM)", capLimit, float64(capLimit)/1e6)
	default:
		return fmt.Sprint(capLimit)
	}
}
