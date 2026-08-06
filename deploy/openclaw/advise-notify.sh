#!/bin/sh
# advise-notify.sh — el notificador de la automejora (ver docs/automejora.md).
#
# Corre `advise --alerts`, compara los fingerprints contra los que ya avisó y
# escribe SOLO lo nuevo. Si no hay nada nuevo imprime NO_REPLY, que es el token
# que OpenClaw usa para suprimir la entrega de un cron job de tipo `--command`:
# una noche tranquila no manda mensaje y no gasta un turno de modelo.
#
# Este archivo se ENTREGA, no se instala solo. La línea de `openclaw cron create`
# que lo engancha está en deploy/openclaw/README.md.
#
# Estado: la lista de fingerprints ya avisados. Vive aquí y no dentro del binario
# a propósito — la herramienta mide, no aprende; acordarse de lo ya dicho es
# trabajo del notificador, no del medidor.
#
# Variables de entorno (todas opcionales):
#   LASM_BIN           ruta del binario            (default: llm-agent-spend-manager en el PATH)
#   LASM_ALERT_WINDOW  ventana del reporte         (default: week)
#   LASM_ALERT_STATE   archivo de fingerprints     (default: $XDG_STATE_HOME/llm-agent-spend-manager/alerts-seen)
#
# Requiere jq. Si falta, o si el binario truena, el script sale distinto de cero
# y el cron lo registra como error en vez de fingir una noche tranquila.
set -eu

BIN="${LASM_BIN:-llm-agent-spend-manager}"
WINDOW="${LASM_ALERT_WINDOW:-week}"
STATE="${LASM_ALERT_STATE:-${XDG_STATE_HOME:-$HOME/.local/state}/llm-agent-spend-manager/alerts-seen}"

command -v jq >/dev/null 2>&1 || {
	echo "advise-notify: falta jq" >&2
	exit 1
}

feed=$("$BIN" advise --window "$WINDOW" --alerts)

mkdir -p "$(dirname "$STATE")"
[ -f "$STATE" ] || : >"$STATE"

seen=$(jq -R -s 'split("\n") | map(select(length > 0))' "$STATE")
fresh=$(printf '%s' "$feed" | jq --argjson seen "$seen" \
	'[.alerts[] | select(.fingerprint as $fp | ($seen | index($fp)) | not)]')

if [ "$(printf '%s' "$fresh" | jq 'length')" -eq 0 ]; then
	# El token silencioso de OpenClaw para jobs `--command`.
	echo "NO_REPLY"
else
	# Las etiquetas de impacto son las MISMAS que pinta el reporte de texto
	# (impactLabel en cmd/llm-agent-spend-manager/advise.go): quien reciba el aviso
	# y luego corra `advise` a mano debe leer la misma palabra en los dos lados.
	printf '%s' "$fresh" | jq -r --arg w "$WINDOW" '
    def impacto: {"high":"ALTO","medium":"MEDIO","low":"BAJO"}[.] // ascii_upcase;
    "llm-agent-spend-manager — automejora · ventana \($w) · \(length) aviso(s) nuevo(s)",
    "",
    (.[] |
      (if .kind == "escalation" then "[BRECHA DE ARQUITECTURA] " else "[\(.impact | impacto)] " end)
        + "\(.id) · \(.title)",
      "  \(.metricName): \((.metric * 1000 | round) / 10)%",
      "  " + (if .kind == "escalation" then "Qué falta: " else "Qué hacer: " end) + .action,
      ""
    ),
    "Evidencia completa: llm-agent-spend-manager advise --window \($w)"
  '
fi

# El estado es una FOTO de lo que el reporte dice hoy, no una bitácora de todo lo
# que alguna vez se avisó: un hallazgo que dejó de salir desaparece del archivo, y
# por eso su regreso vuelve a ser noticia. Se reescribe también en la rama
# silenciosa, o el que se apagó se quedaría marcado como visto para siempre.
printf '%s' "$feed" | jq -r '.alerts[].fingerprint' >"$STATE.tmp"
mv "$STATE.tmp" "$STATE"
