# Ventana de contexto: cuánto le queda a cada sesión antes de chocar (método)

Doc de método, no de código. Explica de dónde sale cada número de la sección
**`CUÁNTO CONTEXTO QUEDA`** de `llm-agent-spend-manager quota`, y qué
deliberadamente **no** se calcula porque no se puede derivar de esta máquina.

Es una dimensión **distinta** a la cuota (`cuota.md`) y al `$`:

| Dimensión | Unidad | Qué se acaba | Doc |
|---|---|---|---|
| Cuota del plan | tokens / USD por ciclo | la ventana de 5 h y el tope semanal de la **cuenta** | `cuota.md` |
| **Ventana de contexto** | **tokens de contexto vivo por sesión** | **el contexto de UNA conversación** | este doc |
| Costo equivalente | USD estimados | nada — es comparativo | `architecture.md` §3.1 |

Se pueden agotar por separado: la cuota puede ir holgada mientras una sesión ya
va al 95% de su ventana y se va a compactar sola en el próximo turno.

## 1. El incidente que originó el ticket (2026-07-26)

Una sesión al **60%** de contexto en Sonnet 5 saltó a **298%** al cambiar a Opus
5, **sin escribir un solo token más**. No fue un bug de conteo: la ventana
efectiva cambia de tamaño con el modelo, así que el mismo contexto vale distinto
porcentaje según contra qué techo se mida.

El mecanismo está documentado por el propio proveedor. El changelog local de
Claude Code (`~/.claude/cache/changelog.md`) trae este renglón:

> Fixed Opus 4.7 sessions showing inflated `/context` percentages and
> autocompacting too early — Claude Code was computing against a 200K context
> window instead of Opus 4.7's native 1M

Es exactamente la misma falla: medir el contexto contra la ventana equivocada.
Por eso aquí **el techo es un dato por modelo, nunca una constante global**.

## 2. El techo por modelo: de dónde sale cada cifra

Vive en `internal/pricing/contextwindow.go`, junto a la tabla de precios, porque
es la misma clase de hecho sobre un modelo (algo que el proveedor decide y
cambia) y porque los dos se usan sobre el mismo turno.

Dos fuentes, las dos **locales y legibles por máquina**, leídas el 2026-07-30:

- **CATÁLOGO OPENCLAW** —
  `node_modules/openclaw/dist/extensions/anthropic/openclaw.plugin.json`, el
  catálogo de modelos que trae el runtime de OpenClaw, con un
  `contextWindow` por id de modelo.
- **CHANGELOG DE CLAUDE CODE** — `~/.claude/cache/changelog.md`, las notas de
  versión del propio Claude Code, que declaran la ventana cuando sale un modelo.

| Modelo | Ventana | Fuente | Pico observado en esta máquina |
|---|---|---|---|
| `claude-opus-4-8` | 1,048,576 | catálogo (el changelog no dice nada de este modelo) | 998,050 |
| `claude-opus-5` | 1,000,000 | changelog: *"now the default Opus model — 1M context"* | 633,948 |
| `claude-sonnet-5` | 1,000,000 | catálogo + changelog (*"a native 1M-token context window"*) | 933,834 |
| `claude-fable-5` | 1,000,000 | catálogo + changelog (*"Fable 5 includes 1M context by default"*) | 292,120 |
| `claude-haiku-4-5` | 200,000 | catálogo (también para el id con fecha) | 60,373 |
| `claude-sonnet-4-6` | **no derivable** | las fuentes se contradicen: catálogo 200,000 vs changelog *"now has 1M context"* | 37,909 |
| `claude-sonnet-4-5` | **no derivable** | no está en el catálogo y el changelog dice que su variante de 1M *"is being removed from the Max plan"*: depende del plan | sin turnos |
| `nemotron-3-super` | **no derivable** | modelo local (Ollama): la ventana la fija el runtime al cargar, no está declarada en la config local | 153,806 |

**Verificación cruzada:** el pico de contexto que un modelo llegó a cargar es un
**piso duro** de su ventana. Ninguno de los picos observados supera la ventana
que la tabla le asigna — el test `TestContextWindowTable_MatchesObservedPeaks`
fija ese cruce, así que si alguien apunta una ventana demasiado chica, truena.
No prueba que la cifra sea exacta (eso solo lo publica el proveedor); atrapa el
error que sí importa.

**Lo que NO se hizo:** derivar la ventana de Opus 5 del incidente. La cuenta
sale redonda (60% de 1M = 600k; 600k / 2.98 ≈ 201k) y habría sido tentador
apuntar 200,000 — pero esta máquina midió **633,948** tokens de contexto en un
solo turno de `claude-opus-5`, o sea que 200k es **imposible**. El 298% salió de
que el indicador estaba midiendo contra la ventana equivocada, no de que la
ventana fuera de 200k. Un número que casa con la anécdota y contradice la
medición no es un dato.

## 3. Qué se mide: contexto vivo por hilo, no por sesión

- **Contexto vivo** = `input + cache-read + cache-write` del **último turno** del
  hilo. Es lo que ese turno realmente cargó, medido por el proveedor, no un
  promedio ni una suma de la sesión. La suma de tokens de una sesión de 4,000
  turnos es 30x su ventana y no significa nada: el contexto es un **nivel**, no
  un acumulado.
- **Por hilo `(sesión, hilo)`, no por sesión.** Una sesión que lanzó subagentes
  corre varios contextos independientes a la vez (`aggregate.Record.ThreadID`);
  mezclarlos daría una curva que no describe a ninguno. Es la misma regla que ya
  usa `internal/contextcurve`.
- **Modelo activo** = el modelo del último turno del hilo. La ocupación se mide
  **siempre contra la ventana de ese modelo**, que es justo lo que hace que
  cambiar de modelo recalcule el porcentaje.
- **Solo tier medido.** Cursor y Antigravity no exponen tokens por turno, así que
  no tienen contexto vivo que medir; salen listados con su razón, nunca con un 0
  que se lea como "va vacía".

## 4. El cambio de modelo a media sesión

Cuando un hilo cambia de modelo, el reporte emite un **cambio de ventana**: el
contexto que cruzó el cambio (el del último turno con el modelo viejo, sin un
token nuevo escrito) medido contra las **dos** ventanas.

```
claude-opus-4-8 → claude-sonnet-5 · 819,451 tokens de contexto sin escribir un token más
  78% de la ventana de claude-opus-4-8 (1,048,576)  →  82% de la de claude-sonnet-5 (1,000,000)
```

Si alguna de las dos ventanas no es derivable, el cambio se reporta **igual**,
con el motivo en lugar del porcentaje que falta: que no se pueda medir el salto
no lo hace menos real.

Los cambios entre dos modelos con **la misma** ventana se cuentan en una línea en
vez de listarse (`sameCeilingShifts`). Pasan todo el tiempo en esta flota —casi
todos sus modelos cargan ~1M— y cinco renglones de `21% → 21%` taparían el único
donde el techo sí se movió.

**Lo que hoy NO se ve en esta máquina:** un salto de la magnitud del incidente
(60% → 298%). En la corrida real de 60 días, 52 cambios de modelo no movieron el
techo, y los que sí lo movieron fueron todos entre 1,048,576 y 1,000,000, o sea
saltos de `78% → 82%`. Por eso las ventanas se imprimen **exactas** y no en
millones: redondeadas las dos a "1.0M", esa línea se leería como un error de
cálculo en vez de como el techo que de verdad se movió. El caso dramático queda
fijado en un **test** (`TestAnalyze_ModelSwitchRescoresTheSameContext`: 600k
tokens = 60% de 1M y 300% de 200k), no simulado en el reporte.

## 5. Avisar ANTES: el umbral

Mismo patrón que el gasto: un umbral de advertencia **configurable** antes de
tocar el techo.

- Env `SPEND_CONTEXT_WARN_PCT`, default **0.80** (se acepta `0.80` o `80`).
- **Por qué 0.80.** Es política declarada, con su razón medida. Dejar 20% de
  ventana libre cubre el turno pesado: sobre 16,952 saltos de contexto turno-a-
  turno reales de esta máquina (2026-07-30), el p99 sube **21,947** tokens y el
  máximo observado sube **79,193**. En la ventana más chica de la tabla (200,000)
  el 20% son 40,000 tokens — alcanza para el p99 con holgura, y no para el peor
  caso. Con un umbral en 0.95 (10,000 tokens de margen) el aviso llegaría después
  del turno que lo rompe, que es exactamente el fallo que el ticket ataca.
- Tres estados: `ok`, `advertencia` (pasó el umbral, no el techo), `techo` (pasó
  el 100%: el runtime ya está compactando o negándose). Sin ventana derivable no
  hay estado: dice `sin % calculable` con el motivo.

## 6. Dónde se lee

En las tres superficies, todas alimentadas por el **mismo** `quota.Analyze`, para
que no puedan discrepar:

| Superficie | Dónde |
|---|---|
| CLI | `llm-agent-spend-manager quota` → sección `CUÁNTO CONTEXTO QUEDA`, justo después de `CUÁNTO QUEDA` |
| HTTP | `GET /api/quota` → campo `contextWindows` (incluye `warnAt`, así que el umbral aplicado viaja con el dato) |
| Dashboard | bloque *Cuánto contexto queda*, arriba de *Quién se la come* |

Va dentro de `quota` y no en un comando nuevo porque `quota` ya es el reporte de
**ventanas que se acaban**; quien está por mandar un turno necesita las dos
respuestas de un tirón (`decisions.md`).

La jerarquía de confianza se respeta igual que en el resto: la ocupación es
cifra **medida** y va sólida; una ventana no derivable imprime su **motivo** donde
iría el porcentaje, nunca un `0`; y los agentes sin tokens por turno se **nombran**
como sin contexto medible en vez de omitirse.

## 7. Lo que este método NO mide (y por qué)

- **La ventana efectiva del runtime.** El changelog de Claude Code dice que
  reserva espacio para los tokens de salida: *"context window blocking limit
  being calculated using the full context window instead of the effective context
  window (which reserves space for max output tokens)"*. Esa reserva no está
  declarada por modelo en ninguna fuente local para todos los modelos de la
  tabla, así que aquí se mide contra la ventana **nativa** y el porcentaje del
  runtime puede leerse un poco más alto que el de este reporte. Se declara en vez
  de asumirse.
- **La compactación futura.** Cuándo va a compactar el runtime es su política, no
  un dato observable en los transcripts.
- **El punto de no-retorno.** Es otra pregunta y ya la contesta
  `internal/contextcurve`: ahí la pregunta es *cuándo sale más barato
  reiniciar*; aquí es *cuánto falta para chocar*. Dos números distintos sobre el
  mismo contexto.
