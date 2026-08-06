# Automejora — medir el uso de tokens y mejorar el uso

Este documento explica **cómo** se calcula el reporte de automejora (`advise`), para que
ninguna cifra tenga que creerse de fe. Misma regla de casa que `calibracion.md`: **no se
inventa ningún número**. Si algo no se puede derivar de los datos, el reporte lo dice en vez
de rellenarlo.

- Código: [`internal/advise`](../internal/advise) (métricas, reglas y escalamiento), [`internal/pricing`](../internal/pricing) (atribución por bucket).
- Superficies: `llm-agent-spend-manager advise [--window today|week|all] [--json|--alerts]`,
  `GET /api/advice?window=…`, y la sección **Automejora** del dashboard.

---

## 1. Por qué "tokens" no es la métrica útil

El `status` responde *cuántos tokens y cuánto costo equivalente*. Eso no alcanza para
optimizar, porque **un token no vale lo que otro token**. Con los precios de lista que ya
vivían en `internal/pricing` (mismo modelo, Opus 5):

| Bucket facturable | Precio por millón | Relativo al cache-read |
|---|---|---|
| `cache-read` | $0.50 | 1x |
| `input` | $5.00 | 10x |
| `cache-write` (5 min) | $6.25 | 12.5x |
| `output` | $25.00 | 50x |

Dicho de otra forma: **10M tokens de cache-read cuestan lo mismo que 200k de output.** Un
reporte que solo suma tokens hace ver un problema donde no lo hay (y esconde el real). Por eso
lo primero que hace `advise` es atribuir el costo equivalente a los cuatro buckets
(`pricing.EstimateByBucket`) y ordenarlos por costo, no por volumen.

## 2. Lo que se mide (mitad "medición")

Todo se calcula sobre los mismos `aggregate.Record` que alimentan el dashboard, ya filtrados
por ventana.

| Métrica | Cómo se calcula | Para qué sirve |
|---|---|---|
| **Costo por bucket** | Σ por bucket de `pricing.EstimateByBucket` sobre turnos medidos con modelo con precio | Decir *cuál* es la palanca |
| **Costo por turno** | costo equivalente ÷ turnos | Métrica de eficiencia (no de volumen) |
| **Tokens por turno** | Σ `PointTokens` ÷ turnos | Tamaño típico de contexto+respuesta |
| **Sesiones / turnos por sesión** | agrupado por `(agente, SessionID)` | Detectar rotación de sesiones |
| **Contexto desde caché** | `cacheRead / (input + cacheRead + cacheWrite)` | Cuánto contexto entra al precio con descuento |
| **Reuso de caché** | `cacheRead / cacheWrite` | Cuántas veces se relee lo que se escribió. **< 1 = escribes caché que nadie lee** |

**Los agentes de tier actividad (Cursor, Antigravity) no tienen estas razones.** No exponen
buckets de tokens, así que `Measured=false` y cada superficie muestra `n/a`/`≈` en vez de un 0
que parecería un dato real. Es la misma regla del resto del proyecto: *rango, no punto*.

## 3. Lo que se compara (mitad "automejora")

La tendencia responde **¿estamos gastando mejor?** — no *¿gastamos menos?*.

- Métrica: **costo equivalente por turno**, no costo total. Trabajar más *debe* subir el total;
  eso no es una regresión y el reporte no lo trata como tal (hay un test que lo fija:
  triplicar los turnos con la misma forma por turno sale `estable`).
- Comparación: media de los **últimos 3 días activos** contra los **3 previos**.
  `meanCostPerTurn` usa Σcosto ÷ Σturnos del bloque, no el promedio de razones diarias — un día
  de 2 turnos no puede pesar igual que uno de 2,000.
- Días **sin actividad no entran en la serie**: un fin de semana tranquilo no cuenta como mejora.
- Banda muerta de **±10%** → `estable`. Fuera de eso, `mejorando` (más barato por turno) o
  `empeorando`.
- Con menos de 6 días activos con costo medible: **`insufficient-data`**, explícito. No se
  extrapola una tendencia de dos puntos.

## 4. Los hallazgos (mitad "tips")

Cada regla vive en [`findings.go`](../internal/advise/findings.go), tiene id estable, y **si no
encuentra nada, se calla**. Un reporte que siempre lista seis problemas enseña a ignorarlo.

| ID | Cuándo dispara | Ahorro que reporta |
|---|---|---|
| **E-01** | Un bucket carga **>50%** del costo equivalente | — (no es desperdicio, es la palanca) |
| **E-02** | Sesiones con `cacheWrite > 0` y `cacheRead == 0` | **Cifra dura**: el sobreprecio de escritura que no compró nada |
| **E-03** | ≥30% de las sesiones mueren en ≤2 turnos | **Tope**: costo de ingerir el prefijo de esas sesiones |
| **E-04** | El modo `cron` carga ≥10% del costo | — |
| **E-05** | Turnos medidos con modelo **sin precio** en la tabla | — (es calidad de dato, no desperdicio) |
| **E-06** | Turnos del modelo más caro con output bajo la **mediana** de la flota | — (solo el operador sabe si un modelo barato bastaba) |
| **E-07** | Hilos que arrastraron contexto **más allá de su punto de no-retorno** (§7) | **Tope**: lo que habría costado cortando ahí, en tokens |

**Semántica de `SavingsUSD`: es un TOPE, no una promesa.** Solo E-02 es dinero probadamente
desperdiciado (se pagó el sobreprecio de caché y nadie la leyó). E-03 asume que ese trabajo
podría haber viajado en una sesión existente, y eso **no siempre es cierto** — una corrida de
cron legítimamente arranca en frío. E-07 es exacto **en tokens** pero no mide lo que cuesta
perder el contexto (§7.4). Los demás hallazgos reportan `0` a propósito: preferimos un cero
honesto a un ahorro inventado.

### Umbrales

Los umbrales (50%, 30%, ≤2 turnos, ±10%, 25%/10% de impacto) son **política, no medición**:
viven como constantes con nombre y con su razón escrita en `advise.go`, no como literales
sueltos en un `if`. Si se cambian, cambia qué se reporta — no cambia ningún dato.

### Clasificación de impacto

`impactOf` compara el costo que toca el hallazgo contra el costo equivalente de la flota:
**≥25% = alto**, **≥10% = medio**, resto **bajo**. Así el lector arregla primero lo caro y no lo
primero de la lista. Excepción: E-05 se topa en *medio* — un hueco de medición no es
desperdicio, pero sí invalida comparaciones, así que no puede quedar hasta el fondo.

## 5. Cuándo un tip deja de ser un tip (brecha de arquitectura)

Un reporte que solo sabe *dar consejos* es estructuralmente incapaz de arreglar cierta clase de
problema — y peor: lo empeora. Cada vez que el mismo hallazgo reincide, su costo tocado y su
impacto **suben**, así que un sistema ingenuo lo promueve **con más fuerza justo cuando la
evidencia dice que el consejo es el remedio equivocado**.

Por eso `advise` clasifica el fallo **antes** de elegir el remedio:

| Clasificación | Señal observable | Remedio | Qué hace el reporte |
|---|---|---|---|
| **Brecha de conocimiento** | El patrón es nuevo, o el consejo está moviendo el número | Consejo | Emitirlo como hallazgo (§4) |
| **Brecha de arquitectura** | El consejo ya se emitió N ventanas seguidas y su métrica no mejoró | **Mecanismo**: script, límite duro, cambio de despacho | **NO** re-emitir el tip. Emitir una alerta con el fierro que falta |

> **Una recomendación ya emitida que reincide no significa "esto importa mucho". Significa "esto
> no se está arreglando". Repetirla con mejor redacción *es* el error.**

Origen: un agente falló tres veces la misma regla ("avisar cuando un worker termina") teniéndola
escrita en su memoria. Lo que la arregló no fue reescribirla mejor, fue un proceso que espera al
worker y lo despierta. La regla generalizada: **un mecanismo le gana a una promesa**, y una
recomendación que reincide está pidiendo mecanismo.

### 5.1 Cómo se detecta la reincidencia (exactamente)

Código: [`escalation.go`](../internal/advise/escalation.go). Un hallazgo escala cuando se cumplen
**las tres** condiciones:

1. **Mismo hallazgo, todas las ventanas.** Una *ventana* son **3 días activos** (`recurrenceBlockDays`,
   el mismo bloque que usa la tendencia). Se toman las **3** ventanas más recientes
   (`recurrenceWindows`) — 9 días activos. Los días sin actividad no entran en la serie, igual que en
   la tendencia: un fin de semana tranquilo no estira una ventana.
2. **Mismo consejo.** Se compara `metricName`, no solo el id: E-01 califica su métrica con el bucket
   (`dominant-bucket-share:cache-read`), así que una ventana donde domina **otro** bucket lleva otra
   recomendación — es consejo distinto, no consejo repetido, y no cuenta para la reincidencia.
3. **La métrica no mejoró.** Cada hallazgo declara la **única** métrica que su recomendación existe
   para mover (`Finding.MetricName` / `Finding.Metric`), y siempre es un **share (0..1)**, nunca un
   costo absoluto: trabajar más sube todos los totales, y *"trabajamos más"* jamás debe leerse como
   *"ignoraron el consejo"*. Si esa métrica bajó más de **10%** (`recurrenceImprovementPct`) entre la
   ventana más vieja y la más nueva, el consejo **está funcionando** y se sigue emitiendo. Plana o
   peor → escala.

| Hallazgo | Métrica que su consejo debe mover |
|---|---|
| E-01 | `dominant-bucket-share:<bucket>` — share del costo en el bucket dominante |
| E-02 | `wasted-cache-cost-share` — share del costo en caché escrita y nunca leída |
| E-03 | `short-session-share` — share de sesiones que mueren en ≤2 turnos |
| E-04 | `cron-cost-share` — share del costo en trabajo programado |
| E-05 | `unpriced-turn-share` — share de turnos medidos sin precio |
| E-06 | `expensive-model-small-turn-cost-share` — share del costo en turnos chicos del modelo caro |

Cuando escala, el hallazgo **sale de `findings` por completo** y aparece en `escalations` con el
**mecanismo** que corresponde. Sacarlo es el punto: si solo se reordenara, su impacto creciente lo
empujaría a la cabeza de la lista de tips justo en el caso donde el tip ya se probó y no sirvió.

### 5.2 Por qué esto NO viola "la herramienta mide, no aprende"

Es una regla dura de este proyecto. Esto se implementó de forma que se note que no la rompe:

- **Es una derivación determinista sobre datos que ya tenemos.** Mismos records → mismas
  escalaciones. Hay un test que lo fija.
- **No hay estado guardado ni memoria entre corridas.** "¿Se emitió este consejo antes?" **no** se
  lee de una bitácora: se **re-ejecutan las mismas reglas** sobre las ventanas anteriores del mismo
  conjunto de records (`replayFindings`, el mismo `findings()` que emite el reporte vivo). Eso hace
  la afirmación *más* fuerte, no más débil: dice que **la condición que produce el consejo lleva N
  ventanas siendo cierta**, lo cual no depende de que alguien haya corrido el reporte esos días.
- **Los umbrales son política declarada**, constantes con nombre y con su razón escrita en
  `advise.go` (`recurrenceBlockDays`, `recurrenceWindows`, `recurrenceImprovementPct`) — no
  literales sueltos y no valores inferidos de nada.
- **El mecanismo que se sugiere es una tabla fija** (`mechanismByFinding`), indexada por id de
  hallazgo. El mismo id devuelve siempre el mismo texto. Nada aquí "decide" ni genera.

El aprendizaje sigue viviendo donde ya vivía: en las decisiones y las reglas, fuera de la
herramienta. Lo único que se añadió es que la herramienta ahora puede **medir que un consejo no
está funcionando** y decirlo, en vez de repetirlo más fuerte.

### 5.3 Qué NO hace

- **No escala sin historia suficiente.** Con menos de 9 días activos no hay 3 ventanas completas y
  la sección simplemente no aparece: "lleva reincidiendo" no es una afirmación que los datos
  soporten todavía. Por eso con `--window today` nunca vas a ver escalaciones.
- **No escala si la métrica base es 0** en la ventana más vieja: sin línea base no hay "¿se movió?"
  que contestar, y quedarse callado le gana a escalar por una división entre cero.
- **No decide por ti.** Nombra la clase de fierro que corresponde; montarlo (y si vale la pena) es
  decisión del operador, igual que `SavingsUSD` es un tope y no una promesa.

Cómo estos hallazgos y escalaciones llegan a un humano **sin que nadie corra el comando** vive en
[`../deploy/openclaw/README.md`](../deploy/openclaw/README.md): aquí se mide, allá se entrega.

## 6. Cómo leerlo (y cómo NO)

- El `$` sigue siendo **costo equivalente estimado**, nunca gasto real cuando los agentes corren
  sobre una suscripción de precio fijo. Sirve para comparar peso relativo y como proxy de cercanía
  al tope de la suscripción.
- El reporte **subestima** mientras E-05 esté abierto (turnos sin precio cuentan tokens pero no
  dinero). Ése es el primer hallazgo que conviene cerrar: no se puede optimizar lo que no se mide.
- **El sobreconteo entre fuentes ya está cerrado.** OpenClaw puede guardar el mismo turno en
  varios `.jsonl` (snapshots de una misma sesión), y el adaptador los sumaba como sesiones
  distintas; hoy se deduplica por id de evento. Vale la pena saberlo porque el efecto es
  **asimétrico**: en una corrida de referencia el sobreconteo era enorme *dentro de esa fuente*
  (+78.2%) pero movía el total del agente menos de un 1%, porque el grueso de sus turnos se lee de
  los transcripts del CLI. Un porcentaje grande sobre una fuente chica no es un total inflado.
- "Output chico" **no** implica "tarea fácil": un buen diagnóstico cabe en dos líneas. E-06
  sirve para elegir qué revisar a mano, no para migrar modelos en automático.
- La tendencia mide **eficiencia por turno**. Un día de trabajo intenso y bien hecho puede subir
  el total y bajar el costo por turno: eso es mejorar.

## 7. La curva de contexto y el punto de no-retorno

Las secciones 1-6 miden **cuánto costó** cada sesión. Esta mide **cuánto cargaba**, que es una
pregunta distinta y es la que faltaba: `cache-read` — el 64% del costo equivalente de esta flota —
escala con **(tamaño del contexto × turnos)**. No con lo que el agente escribe: con cuánta historia
arrastra cada turno. Dos sesiones con el mismo costo total pueden tener formas completamente
distintas, y solo una de las dos se abarata cortándola.

El código vive en [`internal/contextcurve`](../internal/contextcurve/contextcurve.go) y se conecta
al reporte en [`contextcap.go`](../internal/advise/contextcap.go).

### 7.1 Qué se mide (por hilo, no por sesión)

Por cada turno, el **contexto** es `input + cacheRead + cacheWrite`: la ocupación de ventana que el
proveedor facturó ese turno.

La agrupación es por **(sesión, hilo)**, no por sesión. Una sesión de Claude Code escribe los turnos
de sus subagentes bajo el **mismo `sessionId`** (con `agentId`, en `<sesión>/subagents/*.jsonl`), y
cada subagente lleva su propio contexto independiente. Medido en esta máquina el 2026-07-27: la
sesión más cara tenía **4,737 turnos de hilo principal y 7,476 de subagentes** bajo un solo id.
Mezclarlos hace que cada relevo parezca un reinicio y la curva no describe nada. De ahí sale
`aggregate.Record.ThreadID`.

### 7.2 La forma: un serrucho

Lo que aparece en los datos reales es un serrucho. El contexto sube turno a turno hasta que algo
re-arma el prefijo (una compactación, un reinicio), cae a una base chica, y vuelve a subir. Dos
números lo describen:

- **`Baseline`** — dónde aterriza un reinicio. Mediana del primer turno de cada corrida.
- **`GrowthPerTurn`** — qué tan rápido sube. Mediana de la pendiente de cada corrida.

Medianas, no promedios, para que una corrida desbocada no fije ninguno de los dos.

Un **reinicio** es un turno cuyo contexto cae por debajo de la mitad del anterior. El 0.5 no es un
número al tanteo: sobre los 12 transcripts más grandes de esta máquina hay **8 bajadas turno a
turno en ~12,000 turnos de hilo principal** — seis colapsan a ≤0.12 del turno previo (re-armados
reales) y dos son ruido en 0.75 y 0.96. **Nada cae entre 0.12 y 0.75**, así que el corte está en
medio de un hueco vacío y moverlo dentro de ese hueco no reclasifica nada.

### 7.3 El punto de no-retorno

Arrastrar un contexto crecido cuesta `cache-read` **cada turno**; re-armarlo cuesta `cache-write`
**una vez**. El punto de no-retorno es el turno donde lo primero ya superó a lo segundo:

```
cacheRead · growth · t(t−1)/2   >   baseline · cacheWrite
```

El lado izquierdo es el sobreprecio acumulado sobre arrancar en frío (el turno `i` carga
`growth·(i−1)` tokens de más y paga `cache-read` por todos); el derecho es lo que cuesta ese
arranque en frío, una sola vez. Se resuelve en forma cerrada, no recorriendo los turnos: así la
respuesta describe **la forma** de la sesión y no depende de hasta dónde llegó a correr.

Cae solo donde tiene que caer: si el prefijo es más caro de rebuildear, el punto se mueve **más
tarde**; si el contexto crece más rápido, se mueve **más temprano**. Ambas cosas están fijadas con
tests.

**`SavingsUSD`** es el costo de contexto observado (exacto, de los datos) menos lo que habrían
costado esos mismos turnos cortando cada `NoReturnTurn` turnos (contrafactual construido con el
`Baseline` y el `GrowthPerTurn` **de esa misma sesión**, nada prestado de otra).

### 7.4 Lo que esta medición NO sabe (leer antes de usar la cifra)

La cuenta es **exacta en tokens** — esos tokens se pagaron de verdad — pero solo pesa tokens.
Cortar una conversación también **tira lo que ya sabía**, y lo que cueste volver a leer esos
archivos **no está medido aquí**. Por eso `SavingsUSD` es un tope y el hallazgo lleva la advertencia
pegada a la cifra, no escondida en este doc.

Cuando un hilo no da para medir (muy corto, sin crecimiento, o modelo sin precio), `Known` sale
`false` y `Reason` dice cuál de las tres: **se dice que no está, no se estima**.

### 7.5 Para qué sirve

Para que el reporte pueda decir *"esta sesión pasó su punto de no-retorno en el turno 42 y corrió
4,527 turnos más allá; cortarla habría ahorrado $1,313"* en vez de *"haz sesiones más cortas"* —
que es exactamente el consejo que llevaba nueve días sin mover nada. El primero se puede cablear;
el segundo depende de que alguien se acuerde.

El valor de configuración que sale de esta medición vive en
[`../deploy/openclaw/README.md`](../deploy/openclaw/README.md).
