> 🌐 [**Español**](README.md) · **English** (this file)

# llm-agent-spend-manager

Cross-agent LLM spend visibility and control (Claude Code, OpenClaw, Cursor,
Antigravity, and more) — a single Go binary, no npm dependencies, no mandatory
third-party services.

**Status: working and usable today.** Covers 4 agents with a visible confidence hierarchy,
per-mode breakdown, terminal + web dashboard (LAN/phone). It reports in the unit that actually
runs out — **how much of the quota window is left**, not how many equivalent dollars. And
beyond **measuring**, it
reasons about what it measured: it knows when its own advice has stopped working, what shape
each workload has, and whether a metric really changed level or the movement fits inside the
noise. This is not a roadmap: `go build` and it already reports real data from your machine.

## The house rule: no number is invented

**Everything the report asserts is derived from measured data.** When something can't be
derived, the binary says so — `n/a`, `falta el dato` (missing data), `insufficient-data`,
`sin clasificar` (unclassified) — instead of filling it in with an estimate that would read
just like a measurement. There is no fitted model, no ML library, no state kept between runs:
same input records → same report.

That sounds like good intentions until it costs you a figure. Three places where it really bites,
and the result comes out **smaller and more honest**:

- **The counterfactual capped to what was observed.** The per-route plan could claim that moving
  *every* turn of a shape to the cheapest model would have saved a fortune — even when that model
  was only observed carrying a fraction of those turns. That's an extrapolation dressed up as
  arithmetic. With the cap, only the saving over turns the cheap option **already demonstrated it
  could carry** is claimed, and the report **says out loud that it capped, and why**. Method:
  [`docs/workload-classes.md` §5.1](docs/workload-classes.md).
- **A double-digit drop that is not claimed as an improvement.** In the outcome ledger,
  `cost-per-turn` can fall 38% and the verdict still be **`sin cambio de nivel`** (no level
  shift): if the step measures under 1σ against the series' own daily dispersion, it fits inside
  the noise. A report headlining "we cut 38%" would be selling variance as a result. Method:
  [`docs/bitacora-resultado.md` §2](docs/bitacora-resultado.md).
- **A per-model quota weight that gets withdrawn instead of published.** Anthropic does not
  publish how it weighs each model against the Max quota, so it can only be derived from what was
  observed — and frequently a single model clears the evidence bar. A weight is a **ratio against
  another model**, so that lone candidate would report `×1.00`: a tautology wearing a finding's
  clothes. The command prints **`no derivable` with the reason**. Method:
  [`docs/cuota.md` §5](docs/cuota.md).

The same rule is why `SavingsUSD` is always a **ceiling, not a promise**, and why every
attribution carries its `TemporalCaveat`: temporal coincidence is not causality.

- Design: [`docs/architecture.md`](docs/architecture.md)
- Tech stack and rationale: [`docs/tech-stack.md`](docs/tech-stack.md)
- Quota method (window, calibrated ceiling, levers): [`docs/cuota.md`](docs/cuota.md)
- Self-improvement report method: [`docs/automejora.md`](docs/automejora.md)
- Workload shape and per-route plan: [`docs/workload-classes.md`](docs/workload-classes.md)
- Outcome ledger: [`docs/bitacora-resultado.md`](docs/bitacora-resultado.md)
- Optional enforcement (caps): [`docs/enforcement-cableado.md`](docs/enforcement-cableado.md)
- Always-on services (user systemd): [`docs/servicios-permanentes.md`](docs/servicios-permanentes.md)

> Docs under `docs/` are currently maintained in Spanish only, and so is the CLI output pasted
> below.

## Covered agents

Every covered agent runs on a flat-rate subscription, so **nothing it reports is "real
spend"**. There is an explicit **confidence hierarchy**, from most to least exact:

1. **Measured — estimated equivalent cost.** Real tokens from the log × API list price.
   - **Claude Code** — parses the local JSONL transcripts; exact tokens per turn.
   - **OpenClaw** — parses the JSONL sessions; also includes **cron/heartbeat** usage (read from
     `openclaw.sqlite`), which would otherwise not show up at all.
2. **Estimated activity.** The agent exposes neither tokens nor `$`; relative weight is
   inferred from its activity and reported as a **range (not a point)**, marked with `≈` and
   one tier below the measured ones.
   - **Cursor** — activity estimated from the real conversation text + a code-tracking signal.
   - **Antigravity** — activity estimated by step count; **no price** (per-conversation model
     not reliably readable) → reports only tokens/activity, never `$`.

Visual hierarchy, most to least confident: **measured > estimated equivalent cost >
estimated activity**. The dashboard and `status` distinguish the tiers (solid border + single
figure for measured; dotted border + `≈` badge + range for estimated activity).

The hierarchy is **not relaxed downstream**: in the per-route plan, Cursor and Antigravity are
listed as *missing data* with their reason, rather than putting an `≈` up against a measured
figure.

## Per-mode breakdown

Beyond the per-agent total, `status` and `/api/summary` break usage down by **mode**:

- **interactive (chat)** — conversation turns,
- **cron / heartbeat** — automatic background work,
- **editor** — code assistance (Cursor / Antigravity).

## Usage

```bash
go build -o llm-agent-spend-manager ./cmd/llm-agent-spend-manager

./llm-agent-spend-manager status               # text table (today) — --window today|week|all
./llm-agent-spend-manager quota                # what's left of the window, who's eating it, what stretches it
./llm-agent-spend-manager advise               # where cost goes, each workload's shape, what to change
./llm-agent-spend-manager outcome              # which real changes landed, and whether a metric moved
./llm-agent-spend-manager serve                # dashboard on localhost ONLY (http://localhost:4600)
./llm-agent-spend-manager serve --lan          # expose to the local network (0.0.0.0) + access token
./llm-agent-spend-manager serve --lan --qr     # + QR (with token) to open it from your phone
```

**Leave it running?** A meter you have to remember to start only measures the times someone
remembered. Ready-made **user** systemd units (no `sudo`, no root) for the dashboard and the
counting proxy live in `deploy/systemd/` — see **`docs/servicios-permanentes.md`**.

`status` (a snapshot of today's spend):

```
llm-agent-spend-manager — costo equivalente estimado · hoy (desde medianoche local)

AGENTE       COSTO EQUIV.  TOKENS      TURNOS
Claude Code  $12.34        18,500,000  120
OpenClaw     $3.21         4,200,000   40
TOTAL FLOTA  $15.55        22,700,000  160

Por modo:
MODO                COSTO EQUIV.  TOKENS      TURNOS
interactivo (chat)  $15.55        22,700,000  160
```

> ⚠️ **Every output block pasted in this README is an ILLUSTRATIVE EXAMPLE**, with invented,
> rounded figures so you can read the format. They are nobody's measurements. What you get when
> you run the commands comes from your own machine and will not look like this.

**By default `serve` listens on `127.0.0.1` only**: nothing is exposed to the network until you
opt in with `--lan`. With `--lan` the binary generates a random 128-bit **access token** and
requires it on **every** route (dashboard and `/api/*`); without it you get `401`. The token is
printed in the banner and baked into the URL and QR, so a phone still opens the dashboard from a
single scan. You can pin the token with `--token <value>` or the `LASM_TOKEN` environment
variable (handy to reuse the same token across restarts). **Prefer `LASM_TOKEN`:** whatever you
pass via `--token` stays in the process `argv`, where any user on the machine can read it with `ps`.

`serve` flags:

| Flag | Default | What it does |
|---|---|---|
| `--port <n>` | `4600` | TCP port to listen on. |
| `--lan` | off | Binds the local network (`0.0.0.0`) **and** requires a token on every route. |
| `--token <value>` | random (128-bit) | Pins the token instead of generating one. Only applies with `--lan`. |
| `--qr` | off | Prints a QR of the LAN URL (token included). Requires `--lan`. |
| `--cache-ttl <dur>` | `10s` | How long a scan is reused before re-reading disk. `0` disables the cache. |
| `--local` | off | **Deprecated**, no-op (see below). |

> `--local` is still accepted but **deprecated** (loopback is now the default); it is a no-op
> that prints a deprecation notice, so old invocations keep working.

The dashboard is installable as a PWA from a phone browser. There is a desktop wrapper
(Tauri) under [`desktop/`](desktop/).

## The unit that actually hurts: the quota window, not the `$`

If your fleet runs on **flat-price subscriptions**, the `$` was never the scarce resource: it's
an equivalence. What actually runs out — and stops agents mid-task — is the provider's **quota
window**. A 5-hour Claude Max window can run dry in 2–3 hours when several agents work in
parallel, and `rate_limit` events are the only hard signal that it happened.

So `quota` leads with the quota and leaves the `$` at the bottom, under its label. It answers
three questions in that order: **what's left · who's eating it · which lever stretches it most.**

```
$ ./llm-agent-spend-manager quota --days 3

CUÁNTO QUEDA                                  (what's left)

  Anthropic (Claude Max) · ventana de 5 h (Max 5x)
    Periodo     15/01 18:30 → 15/01 23:30
    Consumido   16.4M tokens en 165 turnos
    Ventana     6% (rango 5–10%) del techo (estimado calibrado, dispersión ±24%)
    Ritmo       41.8M tokens/h, medido sobre 198 turnos en los últimos 30 min
    Pronóstico  ⚠ se agota en 3 h 25 min, antes del refill

  Cursor · mes del plan (plan mensual)
    Consumido   $0.00 en 8 turnos (+8 turnos sin consumo legible: el real es mayor)
    Ventana     sin % calculable: ninguno de los 8 turnos del ciclo trae consumo legible;
                un 0% mediría el medidor, no el plan

  Calibración: Techo estimado calibrado con 5 agotamientos observados en esta máquina:
               mediana 279M tokens, rango 159M–339M, dispersión ±24%.

QUIÉN SE LA COME                              (who's eating it)
  Total del periodo: 300.0M tokens en 2,000 turnos · $200.00 costo equivalente estimado

                            TOKENS   %      TURNOS  TOKENS/TURNO
    .openclaw/workspace     150.0M   50.0%  1,000   150,000   OpenClaw
    Develop/mi-proyecto     90.0M    30.0%  700     128,571   Claude Code
    Develop/otro-repo       30.0M    10.0%  200     150,000   Claude Code

QUÉ PALANCA LA ESTIRA MÁS                     (which lever stretches it most)

  [P-01] .openclaw/workspace se lleva 50% de la cuota
      Evidencia: OpenClaw · 150.0M de 300.0M tokens en 1,000 turnos, a 150,000
                 tokens/turno (la mediana de los espacios es 128,571)
      Acción:    [...] la palanca es cortar el contexto que arrastra: sesiones más cortas,
                 resumen en vez de historial completo, o mover ese tráfico a un modelo más ligero.
```

**The finding the command has to shout on its own**: when half the quota goes to a workspace that
isn't code — a long conversation, on the most expensive model, at hundreds of thousands of tokens
per turn — that belongs in the first line, not scattered across single-digit crumbs.

Every provider has **its own quota shape** and is modeled that way: Anthropic in tokens against
a calibrated ceiling (rolling window + weekly cap), Cursor in **USD** against a published
monthly allowance — the one case where the `$` is real money — and Antigravity with no cycle at
all, listed as **unmeterable with its reason**, because an agent missing from the table would
read as "consumes nothing".

Two things this command **refuses** to say, and that's part of the point:

- **How much each model weighs against the quota.** Anthropic doesn't publish it. It can only be
  derived from windows a model dominated (≥80%, minimum 3), and since a weight is a *ratio*
  against another model, with a single qualifying model it prints `no derivable` **with the
  reason** instead of a `×1.00` tautology.
- **How much quota switching models would save.** Lever `P-03` does name a lighter model per turn
  when one exists, but it claims **no saving**: quantifying it would need exactly the weighting
  that couldn't be derived, and part of the per-turn difference is the kind of work each model
  gets, not the model.

The total can be verified against an independent recount of the raw transcripts; the method,
calibration, error bars and how to run that verification are in [`docs/cuota.md`](docs/cuota.md).

## From measuring to reasoning about what was measured

A total is not a lever. The five sections below are **one chain**: measure where the cost goes →
know when the advice you already gave has stopped working → measure the shape of the context so
you can give better advice → classify each workload's shape and compare the route that ran it →
and finally check the changes that actually landed against the metric. Each link exists because
the previous one fell short.

### 1. Measure where the cost goes, not how many tokens were spent

A token is not worth what another token is worth: on the same model an `output` token costs ~50x
a `cache-read` one. Summing tokens points at the wrong lever, so `advise` attributes the
estimated equivalent cost to the four billable buckets and ranks them **by cost**.

```
$ ./llm-agent-spend-manager advise --window all

llm-agent-spend-manager — automejora · todo el historial

EFICIENCIA DE LA FLOTA
  Costo equivalente     $1000.00
  Turnos / sesiones     6,400 / 40  (160.0 turnos por sesión)
  Costo por turno       $0.1563
  Tokens por turno      231,900
  Contexto desde caché  96.6%  (reuso: 33.8x lo escrito)

COSTO EQUIVALENTE POR BUCKET FACTURABLE
  BUCKET       COSTO EQUIV.  %      TOKENS
  cache-read   $640.00       64.0%  1,505,882,353
  cache-write  $230.00       23.0%  44,230,769
  output       $120.00       12.0%  5,166,667
  input        $10.00        1.0%   8,433,333
```

On top of that run the **findings with stable ids (E-01…E-08)**, each with the numeric evidence
behind it so it can be checked instead of trusted. When there's nothing to report, it stays
quiet. The efficiency metric is **cost per turn**, not total: doing more work raises the total
and that is not a regression. Full method in [`docs/automejora.md`](docs/automejora.md).

### 2. When advice stops being advice

A report that can only give tips makes one class of problem worse: every time the same finding
recurs, the cost it touches grows, and a naive system promotes it **harder precisely when the
evidence says the tip is the wrong remedy**.

So `advise` classifies the failure before picking the remedy. A finding emitted for **3
consecutive windows** (of 3 active days) without its metric improving **leaves the tip list
entirely** and shows up as an **architecture gap**, naming the mechanism that's missing:

```
BRECHA DE ARQUITECTURA (esto ya no se arregla con un tip)

  [E-07] 118 sesiones (344 hilos) arrastraron contexto más allá de su punto de no-retorno
        Reincidencia: 3 ventanas de 3 días activos · share del costo en contexto arrastrado de más +14.4%
        Evidencia:    El consejo E-07 se emitió en 3 ventanas seguidas de 3 días activos y su métrica
                      (past-no-return-context-cost-share) no mejoró: 49.3% → 48.1% → 56.3% (+14.4%). Un
                      consejo ya emitido que reincide no significa que importe más; significa que no se
                      está arreglando.
        Qué falta:    El tip ya se dio y las sesiones siguen pasándose de su punto de no-retorno. Fierro: el
                      corte no puede depender de que alguien mire el contexto — se cablea donde se lanza la
                      sesión (compactar o reiniciar al llegar al turno medido) o se baja el tope de contexto
                      del runtime que la corre, que es configuración y no criterio. […]
```

*(illustrative example, line-wrapped to fit)*

This does **not** contradict the "the tool measures, it doesn't learn" rule: there is no ledger of
which advice was emitted. Recurrence is established by **re-running the same rules** over the
earlier windows of the same record set, which makes the claim stronger — *the condition has held
for N windows*, whether or not anyone ran the report on those days. The suggested mechanism comes
from a fixed table indexed by finding id. Detail in
[`docs/automejora.md` §5](docs/automejora.md).

**It won't escalate without enough history:** with fewer than 9 active days the section doesn't
appear at all, and with `--window today` you'll never see escalations.

### 3. The context curve and the point of no return

To give better advice than "keep sessions shorter", something that wasn't being measured had to
be: not *how much a session cost*, but **how much context it was carrying each turn**. That's the
question that matters because `cache-read` — the most expensive bucket in most fleets — scales
with **(context size × turns)**.

The real shape is a sawtooth: context climbs ~1,000 tokens per turn up to the window ceiling,
something compacts it, it drops to a small baseline, and climbs again. Two numbers describe it
(`Baseline`, `GrowthPerTurn`), both **medians** so one runaway run can't set either.

The **point of no return** is the turn where the accumulated surcharge of dragging the context
(`cache-read`, every turn) overtakes what it costs to rebuild the prefix from scratch
(`cache-write`, once). It's solved in closed form, so it describes **the shape** of the session
and doesn't depend on how far it happened to run:

```
CONTEXTO: SESIONES PASADAS DE SU PUNTO DE NO-RETORNO
  AHORRO SI SE CORTA  CORTAR EN  SE PASÓ POR   CURVA DE CONTEXTO
  $180.00             turno 42   4,500 turnos  67,008 +999/turno → pico 999,756 (4 reinicios)
  $75.00              turno 40   2,100 turnos  52,939 +876/turno → pico 998,050 (1 reinicios)
  $46.00              turno 34   1,300 turnos  30,815 +714/turno → pico 984,794 (0 reinicios)
  $29.00              turno 35   950 turnos    34,139 +755/turno → pico 778,621 (0 reinicios)
  $21.00              turno 35   730 turnos    36,619 +778/turno → pico 633,948 (0 reinicios)
  El punto de no-retorno es el turno en que lo acumulado por arrastrar el contexto
  supera lo que cuesta re-armarlo desde cero. Sale de la forma medida de cada sesión.
```

*(illustrative example; the real output also carries a `QUÉ SE PIDIÓ` column with the prompt text
that opened each session)*

Two things this measurement gets right and you should know about:

- **It groups by (session, thread), not by session.** Claude Code writes its subagents' turns
  under the **same `sessionId`**, and each subagent carries its own context. A long session can be
  thousands of main-thread turns plus thousands of subagent turns under one id; mixing them makes
  every handoff look like a restart and the curve describes nothing.
- **The saving is a ceiling, not a promise.** The arithmetic is exact *in tokens* — they were
  really paid — but cutting a conversation also throws away what it already knew, and what it
  costs to re-read those files **is not measured**. The warning is attached to the figure, not
  buried in a doc.

A concrete configuration value falls out of this: **lowering the auto-compaction window** of the
runtime that runs the sessions (for instance from 1,000,000 to 200,000 tokens). That's the
runtime's configuration, not this tool's: the report says how much extra would be paid without
the cap, and whoever operates the machine decides whether to apply it.

### 4. Each workload's shape and the per-route plan (Layers 2 and 3)

Two sessions that cost the same are not the same problem. One that dragged growing context for
four thousand turns gets cheaper by cutting it; one that died on its first turn after writing a
cache nobody read gets cheaper by **not asking for the cache**. Layer 2 classifies each workload
(a *context thread*, the same unit as §3) into one of four shapes, with explicit rules and the
lever that applies to each:

```
FORMA DE LA CARGA (qué palanca aplica a cada una)
  FORMA                       CARGAS  TURNOS  COSTO EQUIV.  %      PALANCA
  Conversación larga          330     45,802  $940.00       94.0%  tope de contexto / corte por tarea
  Ráfaga mecánica             130     1,045   $11.00        1.1%   rutear a modelo o agente barato
  Trabajo de contexto grande  2       2       $0.08         0.0%   no releer: punteros, no archivos
  Disparo único               3       3       $0.88         0.1%   no pedir caché
  Sin clasificar              133     1,795   $48.04        4.8%   — falta el dato, no se fuerza a la forma más cercana
  Una carga = un hilo de contexto (una sesión con subagentes corre varios). 465 de 598 clasificadas.
    · 103 cargas sin clasificar: la carga no cae en ninguna de las cuatro formas: ni corta con contexto
      chico, ni corta con contexto pesado, ni larga pasada de su punto de no-retorno
    · 28 cargas sin clasificar: actividad estimada: un registro por conversación, sin turnos ni buckets
      medidos — no hay con qué medir la forma de la carga
    · 2 cargas sin clasificar: sesión larga sin curva de contexto medible: su modelo no está en la tabla
      de precios o su contexto no creció […]
```

Shapes: *long conversation*, *mechanical burst*, *big-context work*, *single shot*. What doesn't
match is reported **unclassified, with the reason**; it is never rounded to the nearest shape,
because "almost a mechanical burst" is an invented data point. It is normal for a single shape —
long conversation — to concentrate nearly all the cost; that's exactly the headline the command
should hand over without anyone digging.

Layer 3 compares, per shape, what **each route that ran it** charged — and this is where the
observation cap from the house-rule section does its work:

```
PLAN DE AHORRO POR RUTA (contrafactual medido, no opinión)

  Conversación larga — $940.00 en 330 cargas (94.0% del costo equivalente)
        Palanca: Tope de contexto / corte por tarea. El costo escala con (tamaño del contexto × turnos) […]
        RUTA         COSTO EQUIV.  COSTO/TURNO  TURNOS  CARGAS  MODELO DOMINANTE
        OpenClaw     $620.00       $0.1478      31,858  230     claude-opus-4-8 (+4 modelos)
        Claude Code  $320.00       $0.1682      13,944  100     claude-opus-4-8 (+5 modelos)
        Falta el dato · Cursor: solo expone actividad estimada (rango, ≈): no se puede medir la
                        forma de sus cargas, así que no entra a la comparación
        Falta el dato · Antigravity: solo expone actividad estimada (rango, ≈): […]
        Contrafactual por ruta: la opción más barata medida es OpenClaw a $0.1478/turno,
          observada en 31,858 turnos de esta forma; mover 13,944 turnos habría evitado $28.44.
        Contrafactual por modelo: la opción más barata medida es claude-haiku-4-5-20251001 a
          $0.0072/turno, observada en 981 turnos de esta forma; mover 981 turnos habría evitado $14.72.
          Topado por la observación: 44,821 turnos fueron por otra opción, pero solo se puede reclamar
          lo que la barata ya demostró cargar (981 turnos). El resto sería extrapolar, no medir.
```

*(illustrative example: one of the four shapes, with long lines folded)*

Three rules keep it honest: only routes that **ran** that shape (a route with no observation is
*missing data*, never interpolated); only **measured** routes; and **same shape ≠ same
deliverable** — nothing here verifies the cheap route would have delivered the same thing, so
every figure is a ceiling and a hypothesis to test, not an order to move the work. A local model
at $0 cost is **excluded** from the counterfactual, and the report says it excluded it: it costs
$0 because it runs on owned hardware, not because it is more efficient. Thresholds and their
derivation in [`docs/workload-classes.md`](docs/workload-classes.md).

### 5. The outcome ledger: advice → real change → did it move? (Layer 4)

§2 can say *"this isn't improving"*. What it can't say is *"this improved **after** that
change"*, because it never knew which changes were made. `outcome` supplies that half: it reads
the changes that **actually happened** (git commits, merges excluded, plus the `commit`/`done`/
`fix` events from the fleet log) and contrasts them against the metrics' daily series.

```
$ ./llm-agent-spend-manager outcome --window all

llm-agent-spend-manager — bitácora de resultado · todo el historial

CAMBIOS MARCADOS (contra esto se contrasta la métrica)
  Commits           1,200  en 12 repos bajo el directorio escaneado
  Entradas de log   800    de las cuales 400 marcan un cambio real
  Líneas ilegibles  1      no se cuentan (JSON inválido en el log)
  Total de eventos  1,600  del 2025-01-10 al 2026-01-15

¿CAMBIÓ DE NIVEL? (medias comparadas contra su dispersión, tipo CUSUM)
  MÉTRICA                 DÍAS  ANTES    DESPUÉS  Δ         σ AGRUPADA  DESPLAZ.  VEREDICTO
  cost-per-turn           35    $0.1635  $0.1002  -38.7%    $0.0740     -0.9σ     sin cambio de nivel
  cost-share:cache-read   35    13.5%    61.0%    +47.5 pp  17.7%       +2.7σ     SUBIÓ DE NIVEL
  cost-share:output       35    4.9%     12.7%    +7.8 pp   4.9%        +1.6σ     SUBIÓ DE NIVEL
  cost-share:cache-write  35    71.5%    26.6%    -44.9 pp  16.3%       -2.8σ     BAJÓ DE NIVEL
  cost-share:input        35    15.0%    1.1%     -13.9 pp  2.6%        -5.4σ     BAJÓ DE NIVEL
  Se reporta UN cambio de nivel por métrica: el más grande de la serie (el pico del CUSUM).
  Un segundo escalón dentro del mismo periodo NO aparece aquí — acota la ventana para verlo.
```

That first row is the house rule collecting: **−38.7% and still `no level shift`**, because
−0.9σ against the series' own dispersion doesn't distinguish a step from daily noise.

How it works, in two steps you can redo with a calculator: **where** (CUSUM — the extreme of the
accumulated deviation marks where the two regimes meet) and **how much** (the two means against
their **pooled standard deviation**, verdict at `|Δ| > 1.0σ`). There are four verdicts, and
`insufficient-sample` **is a result, not a failure**.

Attribution doesn't guess: candidate changes are those falling between the shift day and 2
**active** days back; if there are several, **they are all listed and none is credited**; if
there are none, that is a real finding (whatever moved it isn't in this ledger). Every
attribution carries the mandatory notice that temporal coincidence is not causality.

And the part that's hardest to write — what **can't** be graded is listed with its reason:

```
  SIN SERIE DIARIA (no se puede calificar, y por eso no se califica)
        E-02 · métrica wasted-cache-cost-share
        E-07 (escalado: ya había dejado de ser un tip) · métrica past-no-return-context-cost-share
        Por qué: esta métrica se define por sesión, y una sesión partida a medianoche no da un valor
        diario honesto — sin serie diaria no hay nivel que comparar.
```

Today **only E-01 has a daily series**, and it's an exact correspondence: its metric *is* a
bucket's share of the estimated equivalent cost. Adding another is one function per metric,
provided it has a defensible daily value. Method in
[`docs/bitacora-resultado.md`](docs/bitacora-resultado.md).

`outcome` reads git and the fleet log, which are I/O from **outside** the usage data — that's why
it is a separate command and not a section of `advise` (`advise.Analyze` is a pure function of
the usage records, and a commit is not a usage record). Paths are configurable with `--repos` and
`--log`.

## Where each thing shows up

Not everything is on every surface, and it's worth knowing before you go looking:

| Section | Terminal | `--json` | HTTP | Dashboard |
|---|:--:|:--:|:--:|:--:|
| Per-agent and per-mode totals | `status` | ✅ | `/api/summary`, `/api/daily` | ✅ |
| Quota window, burn rate and forecast | `quota` | ✅ | ❌ | ❌ |
| Who eats the quota and which lever stretches it | `quota` | ✅ | ❌ | ❌ |
| Billable buckets and trend | `advise` | ✅ | `/api/advice` | ✅ |
| Most expensive tasks | `advise` | ✅ (`topTasks`) | `/api/advice` | ❌ |
| Context past the point of no return | `advise` | ✅ | `/api/advice` | ✅ |
| Findings and architecture gaps | `advise` | ✅ | `/api/advice` | ✅ |
| Workload shape and per-route plan | `advise` | ✅ (`workloads`) | `/api/advice` | ❌ |
| Outcome ledger | `outcome` | ✅ | ❌ | ❌ |

```bash
./llm-agent-spend-manager advise  --window all --json   # for another agent to consume
./llm-agent-spend-manager outcome --window all --json
curl localhost:4600/api/advice                          # and over HTTP, like the rest of the API
```

## What it does NOT do

So the status doesn't read inflated:

- **There is no ML, no model, no learning.** Everything is deterministic derivation over measured
  data: same records → same report, no state between runs. The outcome ledger is precisely the
  **prerequisite** for any future model — it's what would produce the `(change, metric before,
  metric after)` dataset — not a substitute for one.
- **`outcome` has no HTTP route and no dashboard panel.** It's CLI + `--json`. The dashboard today
  renders what `/api/advice` exposes, and `outcome` doesn't go through it.
- **`quota` isn't in the dashboard either, on purpose.** The dashboard still leads with the `$`;
  painting the quota over it before the unit was right would have meant rebuilding the panel
  twice. CLI + `--json` until the dashboard is picked back up.
- **Nobody publishes Anthropic's quota ceiling.** The one `quota` reports is a **calibrated
  estimate** from the exhaustions observed on your machine, with its range and its dispersion —
  a wide range, not a measurement. Below 3 observed exhaustions the command prints no ceiling at
  all.
- **The workload-shape and per-route-plan section isn't in the dashboard either.** It shows up in
  the terminal and under the `workloads` key of `advise --json`.
- **Only E-01 has a daily series.** E-02 and E-07 are defined **per session**, and a session split
  at midnight has no honest daily value: the number would be an artifact of the cut. They're
  listed under `SIN SERIE DIARIA` with the reason instead of being graded badly.
- **Cursor's and Antigravity's workload shape is unmeasurable today.** They expose one record per
  conversation, not per turn: no turns, no buckets, no curve. That's the biggest hole — the
  "route it to a cheaper agent" lever cannot be quantified with the data that exists, and the
  report says so instead of filling it in.
- **The context cap is a recommendation, not something this tool applies.**
- **No reported saving is a promise.** They are all ceilings over observed tokens; none measures
  what losing the context costs, nor verifies the cheap route would have delivered the same.

## Enforcement (optional, off by default)

There is a layer of combined cross-agent **hard caps**: a small proxy the agents go out through,
which counts and blocks when they overrun. **It is off by default** — the visibility MVP works
without it, and it only kicks in if wired on purpose:

```bash
llm-agent-spend-manager proxy                              # counts, never blocks
llm-agent-spend-manager proxy --cap 120000000 --window 5h  # caps at 120M tokens/5h
```

The cap counts **real tokens**: the proxy reads the `usage` the provider reports on every response
(streaming included) and charges `input + output + cache_creation + cache_read` — the same unit the
plan ceiling is measured in. No estimating from bytes: that was tried, and under prompt caching the
estimate drifted ~7×.

**It needs nothing else installed** — no Docker, no Redis, no service: the tally lives in a SQLite
file the tool creates itself, and it survives restarts. The cap uses a **sliding** window, not a
fixed one, so you can't sneak twice the budget across the boundary. (`--state memory` to avoid
touching disk; `--redis` to share a cap across separate machines.) The proxy listens on **loopback
only** and forwards your provider credentials untouched; that's why it is never bound to the
network. How to point each agent at it is in
[`docs/enforcement-cableado.md`](docs/enforcement-cableado.md).

Worth reading alongside §2: the architecture gaps the report escalates name exactly that class of
mechanism — a cap that doesn't depend on anyone remembering.

## Development

```bash
go build ./...
go vet ./...
go test -p 1 ./...    # -p 1: one test process at a time (shared machine)
```

Requires **Go 1.26.5+** (what `go.mod` pins, and what CI uses). The minimum moved up from 1.25
for security: the older Go stdlib shipped vulnerabilities reachable from this binary.
Single binary, no npm dependencies, no mandatory third-party services.

**Branches:** active work goes to **`dev`**; **`main`** is stable and merged from `dev`
when ready — never commit directly to `main`. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

**Next candidate (not urgent):** an adapter for OpenAI's **Codex CLI**. It stores local
JSONL logs with real tokens per turn just like Claude Code, so it would be a **measured** tier
with no estimation.
