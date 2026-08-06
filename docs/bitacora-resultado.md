# Bitácora de resultado (Capa 4) — consejo → cambio real → ¿se movió la métrica?

**Qué cierra.** `internal/advise` ya sabía decir *"esto no está mejorando"*: su chequeo de
reincidencia (`escalation.go`) detecta un consejo que se emitió tres ventanas seguidas sin
mover su propia métrica. Lo que no sabía decir es *"esto mejoró **después de** aquel cambio, y
aquello otro no sirvió"* — porque nunca supo qué cambios se hicieron. Esta capa aporta esa mitad.

**Cómo se corre.** `llm-agent-spend-manager outcome [--window today|week|all] [--json]`, con
`--repos` y `--log` para apuntar a otra máquina.

**Regla de la casa, intacta:** no se inventa ningún número. Cada figura del veredicto es aritmética
sobre datos medidos, y viene en el resultado (`--json`) para que cualquiera la rehaga a mano. No hay
modelo ajustado, no hay librería de ML, no hay estado guardado entre corridas.

---

## 1. Los cambios marcados (`internal/outcome/change.go`)

Los eventos contra los que se contrasta la métrica. Nunca se infieren: existen.

| Fuente | Qué se lee | Detalle |
|---|---|---|
| `git` | `git log --no-merges` de cada repo bajo `--repos` | Se escanea el directorio y un nivel abajo (así entran los proyectos de back+front). Un worktree lleva un `.git` **archivo**, no directorio, y por eso queda fuera: contarlo metería los mismos commits dos veces. |
| `log` | `log.ndjson` de la flota | Solo los `ev` de la allowlist `changeEvents`: `commit`, `done`, `fix`. Un `note`, un `wip`, un `handoff` o un `alert` son mensajes **sobre** el trabajo, no el trabajo aterrizando. |

**Los merges se excluyen a propósito.** Un merge no introduce cambio propio: su contenido son los
commits que trae, y esos ya están en la bitácora. Contar los dos pondría el mismo trabajo dos veces
en dos instantes distintos, y volvería inseparable cualquier ventana.

**Lo que no se pudo leer se cuenta.** Este contador no es defensivo de adorno: en el log que motivó
la capa había decenas de **registros truncados** —sin su llave de cierre— y comillas sin escapar
dentro de `note`. Se saltan **y se reportan**: sin ese contador, *"no hubo cambios marcados en esa
ventana"* y *"no pudimos ver esa parte del log"* se leen idénticos en la página. Por eso se lee
línea por línea y no como un stream de JSON — un decoder de stream se detiene en la primera línea
mala y se lleva con él todas las que siguen. Reparar el archivo baja el contador a cero, pero el
mecanismo se queda: un `log.ndjson` que escriben varios agentes a la vez vuelve a romperse.

## 2. Detección de cambio de nivel (`internal/outcome/levelshift.go`)

Dos pasos, los dos rehacibles con una calculadora.

**DÓNDE — CUSUM.** Se recorre la serie acumulando la desviación de cada día respecto a la media de
toda la serie. Una racha de días arriba de la media empuja la suma hacia arriba y una racha abajo la
jala hacia abajo, así que el extremo de esa acumulación es donde se juntan los dos regímenes. El día
de cambio es el siguiente al pico. Solo se consideran cortes que dejen `minSideDays` (4) días a cada
lado: un punto de cambio con tres días después no es uno que este test pueda evaluar.

**CUÁNTO — dos medias contra su dispersión.** Se compara la media de antes con la de después usando
la **desviación estándar agrupada** (la estimación de dos muestras de siempre: las varianzas de cada
lado pesadas por sus grados de libertad). Es la escala correcta porque mide la variación **dentro**
de cada régimen, no la que el escalón mismo crea. El veredicto sale de
`|Δ| > minShiftStdDevs × σ_agrupada`, con `minShiftStdDevs = 1.0`.

Está escrito como producto y no como división a propósito: así el caso de dispersión cero (un
escalón perfectamente limpio, sin variación diaria en ningún lado) no necesita regla especial, y
nunca aparece un infinito que ningún JSON puede codificar.

**Cuatro veredictos.** `shift-down`, `shift-up`, `no-shift`, `insufficient-sample`. El último **es un
resultado, no un fallo**: con un puñado de días activos a cada lado, que las dos medias difieran no
dice nada, y no decir nada es lo correcto.

**Un solo escalón por métrica.** Se reporta el más grande de la serie (el pico del CUSUM). Un segundo
escalón dentro del mismo periodo no aparece — hay que acotar la ventana para verlo. El reporte lo
dice en la página, porque quien no lo sepa leería `sin cambio de nivel` como *"nunca se movió nada"*.

## 3. Atribución honesta (`internal/outcome/attribution.go`)

**La ventana.** Los cambios candidatos son los que caen entre el día del escalón y
`AttributionLagDays` (2) días **activos** hacia atrás. Se cuenta en días activos, no de calendario,
para que un fin de semana callado no esconda el cambio; el tramo de calendario que eso resuelve se
reporta (`From`/`Through`) para que se pueda verificar el alcance.

**Tres salidas, y ninguna adivina:**

- **Un solo cambio en la ventana** → `Separable`. Es el **único candidato**, lo cual no lo vuelve la
  causa.
- **Varios cambios** → se listan **todos** y no se le acredita a ninguno. Nombrar el más plausible
  sería inventar justo lo que los datos no dan.
- **Ninguno** → el nivel se movió y nada de lo que rastreamos se hizo justo antes. Es un hallazgo
  real: lo que lo movió no está en esta bitácora.

**El texto obligatorio.** Cada atribución lleva `TemporalCaveat`, con estas letras:

> Coincidencia temporal no es causalidad: lo medido es que el nivel de la métrica cambió y que esos
> cambios se hicieron justo antes. Que uno haya causado al otro NO está demostrado aquí, y con estos
> datos no se puede demostrar.

No es un disclaimer de trámite: es lo único que esta capa sabe con certeza sobre su propia salida.

## 4. Qué series se siguen, y por qué son pocas (`internal/advise/outcome.go`)

Cinco, todas sumas **por turno** sobre registros medidos y con precio: `cost-per-turn` y el share del
costo equivalente de cada bucket facturable (`cost-share:cache-read`, `:output`, `:cache-write`,
`:input`). Se calculan con el mismo par `sumBuckets`/`bucketList` del que sale toda otra cifra del
reporte, así que un valor diario y un valor de ventana no pueden discrepar en lo que significan (hay
un test que fija esa igualdad).

**La lista es corta por una razón, no por descuido.** Una métrica definida sobre **sesiones**
(cuántas murieron en dos turnos, cuánto contexto arrastró una conversación) no tiene valor diario
honesto: habría que cortar en dos una sesión que cruza la medianoche, y media sesión se ve corta. El
número diario sería un artefacto del corte y no una medición. Esas métricas **no reciben serie**, y
el reporte lista cuáles y por qué en su sección `SIN SERIE DIARIA` — una bitácora que solo listara lo
que sí pudo calificar se leería como un veredicto sobre el reporte completo.

Hoy solo **E-01** tiene serie, y es una correspondencia exacta y no una aproximación: su métrica *es*
el share del costo equivalente de un bucket. Agregar otra es una función por métrica, siempre que
tenga valor diario defendible.

**Días sin nada medible no entran en la serie.** Un share de cero costo no es cero, es no medible.

## 5. Por qué esto va antes de cualquier ML

Sin esta capa no existe el dataset. Un modelo necesita `(cambio, métrica antes, métrica después)` con
los tres términos medidos, y eso es precisamente lo que la bitácora produce. Construirla es el
prerrequisito, no la alternativa.

## 6. Dónde está cada cosa

| Archivo | Qué |
|---|---|
| `internal/outcome/change.go` | Los cambios marcados: git + `log.ndjson`, con el conteo de lo ilegible. |
| `internal/outcome/levelshift.go` | CUSUM + dos medias contra la dispersión agrupada. |
| `internal/outcome/attribution.go` | La ventana, la separabilidad y la cautela obligatoria. |
| `internal/advise/outcome.go` | El puente: series diarias y qué consejo mide cada una. |
| `cmd/llm-agent-spend-manager/outcome.go` | El comando `outcome` (texto y `--json`). |

`advise.BuildOutcomeLedger` es función pura de `(records, report, changes)`. Los cambios llegan por
parámetro porque son I/O de **afuera** de los datos de uso (git y el log de la flota), y
`internal/advise` no lee archivos — por eso `outcome` es un comando aparte y no una sección de
`advise`: `advise.Analyze` es función pura de los registros de uso, y un commit no es un registro de
uso.
