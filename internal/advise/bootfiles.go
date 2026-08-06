package advise

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Rentheria/llm-agent-spend-manager/internal/bootfiles"
)

// FindingBootFilesOversized names the shared files every agent reads at session
// start that grew past their documented threshold. It is the one finding in this
// package that does NOT come from the usage records: it is measured off the
// filesystem (internal/bootfiles) and attached afterwards by WithBootFiles.
const FindingBootFilesOversized = "E-08"

// MetricOversizedBootFileShare is the share of watched boot files sitting over
// their threshold. Like every metric here it is a share (0..1), so the number
// says "how much of what we watch is out of range" and not "how many files we
// happen to watch this month".
const MetricOversizedBootFileShare = "oversized-boot-file-share"

// WithBootFiles attaches a boot-file measurement to an already-built report and,
// if any watched file crossed its threshold, appends E-08 to the findings.
//
// It is a separate step instead of an argument to Analyze on purpose: Analyze
// answers "what do the usage records say", and it is called from four places
// that have no business touching the filesystem. Keeping the two apart also
// keeps Analyze pure and testable without a disk.
//
// E-08 can never escalate, and that is correct rather than an oversight:
// escalation.go decides recurrence by replaying findings() over past windows,
// and a finding that findings() does not produce never shows up in that replay.
// A file size has no per-window history in the records to replay against, so the
// honest behaviour is for it to stay a tip.
func WithBootFiles(report Report, boot bootfiles.Report) Report {
	report.BootFiles = boot
	if f, ok := bootFilesFinding(boot); ok {
		report.Findings = append(report.Findings, f)
		sortFindings(report.Findings)
	}
	return report
}

// bootFilesFinding fires only when a watched file is actually over its
// threshold. Files that could not be read are not a finding here: they are shown
// in the boot-files section with their reason, and treating "unreadable" as
// "oversized" would raise an alarm the measurement doesn't support.
func bootFilesFinding(boot bootfiles.Report) (Finding, bool) {
	oversized := boot.Oversized()
	measured := boot.MeasuredCount()
	if len(oversized) == 0 || measured == 0 {
		return Finding{}, false
	}

	return Finding{
		ID:    FindingBootFilesOversized,
		Title: fmt.Sprintf("%s por encima de su umbral", filesPhrase(len(oversized))),
		// Capped at medium, like E-05: this is a data-hygiene finding. The cost is
		// real but its size is not derivable from these numbers, and ranking it
		// high on a hunch would push aside findings whose cost IS measured.
		Impact: ImpactMedium,
		Evidence: fmt.Sprintf("%s Cada agente lee estos archivos completos al arrancar cada sesión, "+
			"así que su peso se paga otra vez en cada arranque aunque la tarea del día no toque nada de su contenido. "+
			"El costo en dólares no es derivable: haría falta saber cuántas sesiones abre cada agente y con qué modelo.",
			bootFileEvidence(oversized)),
		Recommendation: "Revisa qué creció. Si es un índice, el detalle probablemente va en su archivo aparte " +
			"(el 2026-07-31, 31 tareas cerradas tenían su nota completa dentro de state.json: migrarlas lo bajó de 69KB a 33KB). " +
			"Si el archivo creció por razones legítimas, sube la línea base en docs/archivos-arranque.md y anota por qué.",
		// No SavingsUSD: see the evidence above. Inventing one would be worse than
		// leaving it at zero.
		MetricName: MetricOversizedBootFileShare,
		Metric:     shareOf(float64(len(oversized)), float64(measured)),
	}, true
}

// bootFileEvidence lists each oversized file with its size, its threshold and —
// when there is an earlier measurement — how much it moved and since when.
func bootFileEvidence(oversized []bootfiles.FileSize) string {
	parts := make([]string, 0, len(oversized))
	for _, f := range oversized {
		part := fmt.Sprintf("%s: %s (umbral %s)", shortPath(f.Path), formatBytes(f.Bytes), formatBytes(f.ThresholdBytes))
		switch {
		case f.HasPrevious && f.DeltaBytes == 0:
			// Ya estaba pasado de umbral la última vez que cambió de tamaño. Decir
			// "-0 B" aquí leería como una medición de cambio; el dato real es que
			// lleva así desde esa fecha.
			part += fmt.Sprintf(", sin cambio desde %s", f.PreviousAt.Format("2006-01-02"))
		case f.HasPrevious:
			part += fmt.Sprintf(", %s desde %s", formatDelta(f.DeltaBytes), f.PreviousAt.Format("2006-01-02"))
		default:
			part += ", sin medición previa para comparar"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ") + "."
}

// filesPhrase keeps the title readable for the common case of a single file.
func filesPhrase(n int) string {
	if n == 1 {
		return "1 archivo de arranque"
	}
	return fmt.Sprintf("%d archivos de arranque", n)
}

// shortPath trims the file to its last two segments: the full path adds nothing
// to a line whose point is which file grew.
func shortPath(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) <= 2 {
		return path
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// formatBytes renders a size in KB when it is big enough for that to read
// better, which for these files it always is in practice.
func formatBytes(n int64) string {
	const kb = 1024
	if n < kb {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/kb)
}

// formatDelta keeps the sign, because "-4.0 KB" and "+4.0 KB" are opposite news.
// Callers phrase a zero delta in words instead (see bootFileEvidence): "-0 B"
// reads like a measured shrink.
func formatDelta(delta int64) string {
	switch {
	case delta > 0:
		return "+" + formatBytes(delta)
	case delta < 0:
		return "-" + formatBytes(-delta)
	default:
		return "sin cambio"
	}
}

// sortFindings is the ordering findings() applies, extracted so a finding
// appended after the fact lands in the same place it would have if it had been a
// rule: by impact first, then by the cost it would avoid.
func sortFindings(out []Finding) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Impact != out[j].Impact {
			return impactRank(out[i].Impact) < impactRank(out[j].Impact)
		}
		return out[i].SavingsUSD > out[j].SavingsUSD
	})
}
