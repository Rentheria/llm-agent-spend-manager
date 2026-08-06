package main

import (
	"fmt"
	"io"

	qrcode "github.com/skip2/go-qrcode"
)

// printQR writes an ASCII QR code for url to out (half-block characters, scans
// straight from a phone camera), preceded by the URL in plain text as a
// fallback for terminals that can't render it. Used by `serve --qr` so the
// dashboard opens on the phone without typing an IP (architecture.md §4.3).
func printQR(out io.Writer, url string) error {
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nEscanea para abrir en el celular:  %s\n\n", url)
	// ToSmallString uses half-block glyphs so the code stays compact enough to
	// fit a normal terminal; false = dark modules on the terminal background.
	fmt.Fprint(out, q.ToSmallString(false))
	return nil
}
