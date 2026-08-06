package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
	"github.com/Rentheria/llm-agent-spend-manager/internal/bootfiles"
	"github.com/Rentheria/llm-agent-spend-manager/internal/lan"
	"github.com/Rentheria/llm-agent-spend-manager/internal/outcome"
	"github.com/Rentheria/llm-agent-spend-manager/internal/server"
)

const defaultPort = 4600

// tokenEnvVar is the environment variable read for the --lan access token when
// no --token is passed, so the token can be pinned without appearing in the
// process argv.
const tokenEnvVar = "LASM_TOKEN"

// cmdServe starts the HTTP server. It binds loopback (127.0.0.1) by default, so
// nothing is exposed to the network unless the user opts in with --lan. With
// --lan the server binds all interfaces AND requires an access token (generated
// unless --token/LASM_TOKEN is set); the token is baked into the LAN URL and QR
// so a phone still opens the dashboard from a single scan.
func cmdServe(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(out)
	port := fs.Int("port", defaultPort, "TCP port to listen on")
	lanMode := fs.Bool("lan", false, "expose the dashboard to the local network (0.0.0.0); requires an access token")
	tokenFlag := fs.String("token", "", "access token for --lan (default: a random one is generated; or set "+tokenEnvVar+")")
	localOnly := fs.Bool("local", false, "(deprecated) loopback is now the default; kept as a no-op for compatibility")
	qr := fs.Bool("qr", false, "print a scannable QR of the LAN dashboard URL (requires --lan)")
	cacheTTL := fs.Duration("cache-ttl", 10*time.Second, "how long to reuse a scan before rescanning (0 disables the cache)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(out, "cannot resolve home directory:", err)
		return 1
	}

	// --local always meant "loopback only", which is now the default; accept it
	// so old invocations keep working, but nudge users toward the new flags.
	if *localOnly {
		fmt.Fprintln(out, "aviso: --local está obsoleto; loopback ya es el default. Usa --lan para exponer a la red.")
	}

	// Loopback by default; --lan is the explicit opt-in to bind the LAN, and it
	// pulls in a mandatory access token.
	host := "127.0.0.1"
	var token string
	if *lanMode {
		host = "" // net.JoinHostPort("", port) => bind all interfaces (0.0.0.0)
		token = *tokenFlag
		if token == "" {
			token = os.Getenv(tokenEnvVar)
		}
		if token == "" {
			token, err = generateToken()
			if err != nil {
				fmt.Fprintln(out, "no pude generar el token de acceso:", err)
				return 1
			}
		}
	}

	// CollectSnapshot is the same read the CLI `quota` command takes: turns plus
	// the session-limit signals calibration needs. Outcome reuses the CLI's
	// default paths for repos and the fleet log.
	loader := func() (aggregate.Snapshot, error) { return aggregate.CollectSnapshot(homeDir) }
	changes := func(ctx context.Context) (outcome.ChangeLedger, error) {
		return outcome.CollectChanges(ctx,
			filepath.Join(homeDir, defaultReposDir),
			filepath.Join(homeDir, defaultLogPath))
	}
	// Read-only by design: the dashboard measures the boot files but never
	// advances their snapshot. Only the CLI `advise` command writes it, so the
	// delta keeps meaning "since the size last changed" instead of resetting on
	// every browser poll.
	bootFiles := func() (bootfiles.Report, error) {
		report, _, err := bootfiles.Collect(os.Getenv, homeDir, time.Now())
		return report, err
	}
	srv := server.New(loader,
		server.WithToken(token),
		server.WithCacheTTL(*cacheTTL),
		server.WithChangesLoader(changes),
		server.WithBootFilesLoader(bootFiles),
	)

	addr := net.JoinHostPort(host, fmt.Sprint(*port))

	lanIP := lan.IPv4()
	for _, line := range serveBanner(*port, *lanMode, lanIP, token) {
		fmt.Fprintln(out, line)
	}

	// The QR points at the LAN URL (the whole reason to scan it from a phone),
	// so it only makes sense when we're actually listening on the LAN.
	if *qr && *lanMode {
		if lanIP == "" {
			fmt.Fprintln(out, "\n(no puedo generar el QR: sin IPv4 de LAN detectada)")
		} else if err := printQR(out, lanDashboardURL(lanIP, *port, token)); err != nil {
			fmt.Fprintln(out, "\n(no pude generar el QR:", err, ")")
		}
	} else if *qr && !*lanMode {
		fmt.Fprintln(out, "\n(--qr se ignora sin --lan: el QR es para abrir desde otro dispositivo en la LAN)")
	}

	if err := srv.Serve(addr); err != nil {
		fmt.Fprintln(out, "server stopped:", err)
		return 1
	}
	return 0
}

// generateToken returns a URL-safe, 128-bit random access token (crypto/rand).
func generateToken() (string, error) {
	b := make([]byte, 16) // 128 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// lanDashboardURL builds the dashboard URL for a LAN client, carrying the token
// in the query when one is set so a single QR scan authenticates the phone.
func lanDashboardURL(lanIP string, port int, token string) string {
	url := fmt.Sprintf("http://%s:%d", lanIP, port)
	if token != "" {
		url += "/?token=" + token
	}
	return url
}

// serveBanner builds the startup lines: title, disclaimer, and the URLs to open.
// In LAN mode it prints the LAN URL (when an IPv4 exists) and the access token,
// and — because auth then applies to every request, including from localhost —
// both URLs carry the token in the query so they work as-is.
func serveBanner(port int, lanMode bool, lanIP, token string) []string {
	query := ""
	if token != "" {
		query = "/?token=" + token
	}
	lines := []string{
		fmt.Sprintf("llm-agent-spend-manager — %s (NO es gasto real)", aggregate.CostLabel),
		fmt.Sprintf("Dashboard:  http://localhost:%d%s", port, query),
	}
	if lanMode {
		if lanIP != "" {
			lines = append(lines,
				fmt.Sprintf("En la red:  http://%s:%d%s   (ábrelo desde el celular en la misma Wi-Fi)", lanIP, port, query))
		} else {
			lines = append(lines, "En la red:  (sin IPv4 de LAN detectada; solo localhost por ahora)")
		}
		lines = append(lines,
			fmt.Sprintf("Token:      %s   (requerido en cada request; ya va incluido en las URLs de arriba)", token))
	} else {
		lines = append(lines, "Modo:       solo localhost (usa --lan para exponer a la red con token)")
	}
	return lines
}
