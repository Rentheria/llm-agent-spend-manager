package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Rentheria/llm-agent-spend-manager/internal/aggregate"
)

func TestWriteStatus_TableAndDisclaimer(t *testing.T) {
	byAgent := []aggregate.AgentTotals{
		{Agent: aggregate.AgentClaudeCode, Totals: aggregate.Totals{TotalTokens: 1234567, CostUSD: 12.5, Turns: 40, UnpricedTurns: 3}},
		{Agent: aggregate.AgentOpenClaw, Totals: aggregate.Totals{TotalTokens: 2000, CostUSD: 0.02, Turns: 5}},
	}
	grand := aggregate.Totals{TotalTokens: 1236567, CostUSD: 12.52, Turns: 45, UnpricedTurns: 3}
	byMode := []aggregate.ModeTotals{
		{Mode: aggregate.ModeCron, Totals: aggregate.Totals{TotalTokens: 2000, CostUSD: 0.02, Turns: 5}},
		{Mode: aggregate.ModeInteractive, Totals: aggregate.Totals{TotalTokens: 1234567, CostUSD: 12.5, Turns: 40, UnpricedTurns: 3}},
	}

	var buf bytes.Buffer
	writeStatus(&buf, aggregate.WindowToday, byAgent, byMode, grand)
	got := buf.String()

	for _, want := range []string{
		aggregate.CostLabel, "hoy", aggregate.AgentClaudeCode, aggregate.AgentOpenClaw,
		"$12.50", "1,234,567", "TOTAL FLOTA", "(+3 sin precio)", aggregate.CostDisclaimer,
		"Por modo:", "cron / heartbeat", "interactivo (chat)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q\n---\n%s", want, got)
		}
	}
	// The only permitted mention of "gasto real" is the disclaimer negating it.
	if !strings.Contains(got, "NO es gasto real") {
		t.Errorf("status output must carry the 'NO es gasto real' disclaimer:\n%s", got)
	}
}

func TestWriteStatus_EmptyWindow(t *testing.T) {
	var buf bytes.Buffer
	writeStatus(&buf, aggregate.WindowWeek, nil, nil, aggregate.Totals{})
	got := buf.String()
	if !strings.Contains(got, "Sin actividad") {
		t.Errorf("empty window should say 'Sin actividad':\n%s", got)
	}
	if !strings.Contains(got, aggregate.CostDisclaimer) {
		t.Errorf("empty window should still show the disclaimer:\n%s", got)
	}
}

func TestWriteStatus_ActivityRowsShowRangeAndLegend(t *testing.T) {
	byAgent := []aggregate.AgentTotals{
		{Agent: aggregate.AgentClaudeCode, Totals: aggregate.Totals{
			TotalTokens: 1000, TokensLow: 1000, TokensHigh: 1000,
			CostUSD: 10, CostLowUSD: 10, CostHighUSD: 10, Turns: 5,
		}},
		{Agent: aggregate.AgentCursor, Totals: aggregate.Totals{
			TotalTokens: 3000, TokensLow: 2000, TokensHigh: 4000,
			CostUSD: 3, CostLowUSD: 2, CostHighUSD: 4, Turns: 2, ActivityTurns: 2,
		}},
	}
	grand := aggregate.Totals{
		TotalTokens: 4000, TokensLow: 3000, TokensHigh: 5000,
		CostUSD: 13, CostLowUSD: 12, CostHighUSD: 14, Turns: 7, ActivityTurns: 2,
	}

	var buf bytes.Buffer
	writeStatus(&buf, aggregate.WindowAll, byAgent, nil, grand)
	got := buf.String()

	for _, want := range []string{
		"≈$2.00–$4.00",       // Cursor cost range
		"≈2,000–4,000",       // Cursor token range
		"$10.00",             // measured Claude Code stays a single figure
		"actividad estimada", // legend present because ActivityTurns > 0
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q\n---\n%s", want, got)
		}
	}
}

func TestThousands(t *testing.T) {
	cases := map[int]string{0: "0", 42: "42", 1000: "1,000", 1234567: "1,234,567", -1500: "-1,500"}
	for in, want := range cases {
		if got := thousands(in); got != want {
			t.Errorf("thousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseWindow(t *testing.T) {
	cases := map[string]aggregate.Window{
		"today": aggregate.WindowToday, "week": aggregate.WindowWeek,
		"all": aggregate.WindowAll, "": aggregate.WindowToday, "bogus": aggregate.WindowToday,
	}
	for in, want := range cases {
		if got := parseWindow(in); got != want {
			t.Errorf("parseWindow(%q) = %q, want %q", in, got, want)
		}
	}
}
