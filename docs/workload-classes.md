# Forma de la carga y plan por ruta (Capas 2 y 3)

Qué mide, con qué reglas, de dónde salió cada umbral y qué NO puede decir. El código vive en
`internal/workload/`, el enganche en `internal/advise/workload.go`, el render en
`cmd/llm-agent-spend-manager/workload.go`.

---

## 1. Por qué una forma y no un total

Dos sesiones que costaron lo mismo **no son el mismo problema**. Una que arrastró un contexto
creciente durante cuatro mil turnos se abarata cortándola; una que murió en su primer turno
después de escribir una caché que nadie leyó se abarata **no pidiendo la caché**. Un reporte que
no distingue las dos solo puede decir "gasta menos", que no es una palanca.

Capa 2 clasifica cada carga por su forma. Capa 3 compara lo que esa forma costó por cada **ruta**
que la corrió. Las dos son derivaciones sobre datos medidos: no hay modelo, no hay clustering, no
se predice nada que no haya corrido.

---

## 2. La unidad: un hilo de contexto, no una sesión

Se clasifica **(sesión, hilo)**, no la sesión. Una sesión que lanzó subagentes corre varios
contextos independientes a la vez; una forma calculada sobre los turnos entremezclados no describe
a ninguno. Es la misma unidad que usa la curva de contexto (`aggregate.Record.ThreadID`, ver `docs/ventana-contexto.md`).

En el reporte esa unidad se llama **carga**.

---

## 3. Las cuatro formas (reglas explícitas, primera que casa gana)

| Forma | Firma medible | Palanca |
|---|---|---|
| **Disparo único** | 1 turno · escribió caché · nadie la leyó | no pedir caché (es E-02) |
| **Trabajo de contexto grande** | ≤ 20 turnos · ≥ 150,000 tokens/turno | no releer: punteros, no archivos |
| **Ráfaga mecánica** | ≤ 20 turnos · < 50,000 tokens/turno | rutear a modelo o agente barato |
| **Conversación larga** | > 20 turnos · pasó **su propio** punto de no-retorno · el contexto crece · cache-read > 50% del contexto | tope de contexto / corte por tarea |

**El orden importa en un solo lugar:** un disparo único también es corto y chico, así que si la
regla de ráfaga se evaluara primero apuntaría a "rutéalo más barato" cuando el desperdicio real es
el sobreprecio de caché que nadie amortizó. Hay un test que fija exactamente eso.

**Lo que no casa se reporta `sin clasificar`, con la razón.** Nunca se redondea a la forma más
cercana: "casi una ráfaga mecánica" es un dato inventado.

Razones posibles de `sin clasificar`:

- **actividad estimada** — Cursor y Antigravity exponen **un registro por conversación**, no por
  turno (`architecture.md` §3.3). No hay turnos, ni buckets, ni curva: ninguna de las features
  que leen las reglas existe para ellos.
- **sin curva medible** — sesión larga cuyo modelo no está en la tabla de precios, o cuyo contexto
  no creció: no hay punto de no-retorno contra el cual compararla.
- **entre formas** — medida, pero sus features caen en el hueco entre las cuatro.

---

## 4. De dónde salió cada umbral

Los umbrales son **política** — una raya trazada sobre una distribución medida —, no mediciones.
Viven todos juntos en `internal/workload/classify.go` con esta misma justificación al lado.
La distribución sobre la que se trazaron —**48,026 turnos / 596 hilos de contexto**, el historial
completo de la máquina donde se derivaron— se cita abajo como la observación que los justifica. No
son constantes universales: si tu flota trabaja distinto, la tabla de `classify.go` es el lugar
donde se re-trazan, con la nueva observación al lado.

| Umbral | Valor | Observación de la que salió |
|---|---|---|
| `fewTurnsCeiling` | 20 turnos | El punto de no-retorno que deriva `internal/contextcurve` tiene **p25 = 19** y **mediana = 24** sobre 524 hilos medibles. A 20 turnos, tres de cada cuatro sesiones **todavía no llegaron a su propio punto de corte**: "córtala antes" no es la palanca disponible, así que la forma la decide lo que carga, no cuánto duró. |
| `smallContextTokensPerTurn` | 50,000 tok/turno | El turno mediano de la flota carga **54,293 tokens** (568 hilos medidos). Un hilo corto por debajo de 50k carga menos que un turno típico: le cabe entero a un modelo más barato. |
| `bigContextTokensPerTurn` | 150,000 tok/turno | Entre los hilos que nunca llegaron a su punto de no-retorno, **p95 = 95,266** y el máximo es **198,816**. A 150k un solo turno carga más que el **pico mediano** de una conversación de 21–100 turnos (**74,690**): la carga es el contexto, no la conversación. |
| `cacheReadDominanceShare` | 50% del contexto | Entre los 351 hilos pasados de su punto de no-retorno, cache-read es la **mediana 91%** del contexto y **64% en p10**. Una mayoría simple separa la forma acumulativa sin cortarla por dentro. |

El límite de "conversación larga" **no es un número global**: es el punto de no-retorno **de cada
sesión**, que `internal/contextcurve` ya deriva de su propia forma medida. Con eso, la clase que
suele cargar la mayor parte del costo queda definida sin inventar ninguna constante.

---

## 5. Capa 3 — el plan por ruta

Para cada forma, lo que cobró cada ruta que la corrió (`$` total, `$`/turno, turnos, cargas, modelo
dominante). Tres reglas la mantienen honesta:

1. **Solo rutas que corrieron esa forma.** Una ruta sin observación se reporta como **falta el
   dato**, con la razón — nunca se interpola desde lo que hizo en otra forma.
2. **Solo rutas medidas.** Cursor y Antigravity son *actividad estimada* (rango, `≈`). Poner su
   cifra al lado de una medida como si pesaran igual es exactamente el error que la jerarquía de
   confianza existe para evitar (`architecture.md` §3.3). Se listan como falta-de-dato, con
   la razón, no se omiten.
3. **Misma forma ≠ mismo entregable.** La cuenta es exacta en tokens observados, pero nada aquí
   verifica que la ruta barata hubiera entregado lo mismo. Toda cifra es un **tope y una hipótesis
   a probar**, no una orden de mover el trabajo.

### 5.1 El tope de observación (lo que separa derivar de predecir)

El contrafactual se calcula sobre `min(turnos que fueron por otra opción, turnos que la opción
barata **realmente cargó**)`.

Sin ese tope, la primera corrida sobre datos reales decía que mover 44,373 turnos a Haiku habría
ahorrado **$6,673** — habiendo observado a Haiku cargando **981** turnos de esa forma. Eso es una
extrapolación de 45× disfrazada de aritmética, y es justo lo que la regla de la casa prohíbe. Con
el tope, la misma corrida reclama **$147.49** sobre esos 981 turnos observados, y el reporte dice
en voz alta que topó y por qué.

### 5.2 Costo equivalente cero: fuera de la comparación

Un modelo local (`nemotron-3-super`) cuesta $0 porque corre en hardware propio, **no por ser más
eficiente**. Dejarlo ganar el contrafactual produciría un "ahorra el 100%" que no dice nada, así
que se excluye y se dice que se excluyó.

### 5.3 Dos lecturas de los mismos turnos

Se emiten dos contrafactuales por forma: **por ruta** y **por modelo**. Son dos lecturas de los
**mismos** turnos, **no se suman**.

---

## 6. Lo que este diseño NO puede decir

- **Nada sobre lo que no corrió.** Si una ruta nunca corrió una conversación larga, el reporte dice que
  falta el dato. No hay estimación posible y no se intenta.
- **Nada sobre equivalencia de entregable.** Ver §5 regla 3. Es el límite duro de todo el plan.
- **Nada sobre las rutas de actividad estimada.** Mientras Cursor y Antigravity expongan un
  registro por conversación, su forma de carga es inmedible. Ése es hoy el hueco más grande: la
  palanca "rutéalo a un agente barato" **no se puede cuantificar** con los datos que hay, y el
  reporte lo dice en vez de rellenarlo.

---

## 7. Cómo verlo

```bash
llm-agent-spend-manager advise --window all          # secciones FORMA DE LA CARGA y PLAN DE AHORRO POR RUTA
llm-agent-spend-manager advise --window all --json   # mismo contenido bajo la llave "workloads"
```
