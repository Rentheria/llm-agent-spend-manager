# Cuota: la unidad que sí duele (método del comando `quota`)

Doc de método, no de código. Explica de dónde sale cada número que imprime
`llm-agent-spend-manager quota`, qué está medido, qué está calibrado, y qué
deliberadamente **no** se calcula porque no se puede.

El corolario normativo que ordena todo esto ("la unidad primaria es la ventana de
cuota, no el `$`") vive en `docs/architecture.md` §3.1, y este comando lo respeta
al pie de la letra.

## 1. Por qué la unidad tenía que cambiar

El reporte de esta herramienta encabezaba con un `$`. **Ese número no es dinero**
cuando los agentes corren sobre una cuenta de suscripción:

- Una cuenta `claude_max` con `hasExtraUsageEnabled: false` no admite cobro por
  token ni overage posible. Si además no hay `ANTHROPIC_API_KEY` en la máquina, no
  existe una sola ruta por la que ese `$` se cobre.
- Cursor sí publica una mesada mensual en USD. Ahí el `$` es dinero, pero es un
  tope mensual fijo, no un consumo por token.

Lo que sí duele es otra cosa: **la ventana de 5 h se acaba en 3 h y los agentes se
quedan a media tarea.** Los eventos de `rate_limit` en los transcripts son la única
señal dura de que pasó, y dos de los cinco agotamientos de la muestra de referencia
(§4) murieron a las **2.4 h** y **3.2 h** de abierta la ventana.

Así que el `$` **no se borra**: baja a cifra secundaria, siempre bajo su rótulo
obligatorio *costo equivalente estimado*, nunca *gasto real*. La cifra primaria
pasa a ser **cuánto de la ventana queda y a qué ritmo se está yendo**.

## 2. Las preguntas que el comando contesta

La salida tiene estas secciones, en este orden, porque es el orden en que se
necesitan:

| Sección | Pregunta |
|---|---|
| `CUÁNTO QUEDA` | ¿Qué % de la ventana llevo consumido y cuánto tiempo me deja el ritmo actual? |
| `CUÁNTO CONTEXTO QUEDA` | ¿Cuánto le falta a cada conversación para llenar la ventana de contexto de su modelo activo? (método en `ventana-contexto.md`) |
| `QUIÉN SE LA COME` | ¿Por espacio de trabajo, modelo, agente y sesión — quién? |
| `QUÉ PALANCA LA ESTIRA MÁS` | ¿Qué cambio concreto la alarga, y cuánto (o por qué no se puede decir cuánto)? |

Las dos primeras son **dos techos distintos**: la cuota se agota para toda la
cuenta y el contexto se agota para **una** conversación, pero los dos paran el
trabajo a media tarea, y quien está por mandar un turno necesita las dos
respuestas de un tirón. Por eso el contexto se lee aquí y no en un comando
aparte.

## 3. Cómo se reconstruye la ventana de 5 h

Anthropic **no expone la ventana en ningún lado**: no hay header, contador ni
endpoint para planes de suscripción. Lo único que expone, al negarse, es el reloj
del refill:

```
You've hit your session limit · resets 12:30pm (America/Mexico_City)
```

Con eso hay suficiente para reconstruir **y verificar**:

- **Reconstrucción** (`internal/quota.SessionWindows`): el primer turno enviado
  sin ventana viva abre una nueva, que dura `WindowLength = 5 h` (el único número
  no medido aquí: es la duración documentada de la ventana rodante). La **fase**
  sí se mide, no se asume.
- **Todos los agentes de la cuenta caen en la misma ventana.** La cuota es **por
  cuenta**, no por agente ni por modelo: observado cuando un solo agotamiento calló
  a dos agentes al mismo tiempo, con toda la cadena de fallback de tres modelos
  muriendo junta.
- **Verificación**: se compara el reset que predice la reconstrucción contra el
  que anunció el proveedor (`ResetDrift`). Sobre las 5 negativas de la muestra de
  referencia el desvío fue de **6 s**, **1.1 min**, **3.8 min**, **7.8 min** y
  **45.7 min**.

Ese último desvío de 46 min es la razón de que cada ventana se trate como
**observación con error**, no como hecho. No se corrige ni se descarta: entra al
cálculo con su desvío registrado.

## 4. El techo: calibrado desde agotamientos reales, jamás publicado

Anthropic no publica el techo de la ventana. La única verdad de campo disponible
es *"en el instante T la cuenta ya estaba vacía"*, así que el techo se **calibra**
desde las negativas que la propia máquina coleccionó (`internal/quota.Calibrate`).

La muestra de referencia de abajo es la que se usa en los ejemplos de este doc.
**No es una constante del programa**: cada instalación calibra con las suyas.

| Ventana abierta (UTC) | Agotada | Sobrevivió | Desvío del reset | Tokens al morir | Modelo dominante |
|---|---|---|---|---|---|
| 2026-06-26 22:41 | 03:34 | 4.89 h | 1.1 min | 279.2M | `claude-opus-4-8` |
| 2026-06-28 14:27 | 18:16 | 3.81 h | 7.8 min | 338.7M | `claude-opus-4-8` |
| 2026-07-04 13:53 | 18:44 | 4.84 h | 3.8 min | 285.1M | `claude-opus-4-8` |
| 2026-07-17 02:54 | 05:20 | **2.44 h** | 45.7 min | 213.5M | `claude-sonnet-5` |
| 2026-07-27 13:30 | 16:40 | **3.17 h** | 0.1 min | 159.4M | `claude-opus-5` |

De esa muestra sale el techo que imprimiría el comando: **mediana 279M tokens,
rango 159M–339M, dispersión ±24%**. Reglas que gobiernan esa cifra, todas política
declarada con su razón al lado en `calibrate.go`:

- `minCalibrationObservations = 3` — con menos de tres, una mediana es **un solo
  dato disfrazado de estadística**. Por debajo de ese piso `Capacity.Known` se
  queda en `false` y el comando dice cuántas observaciones faltan, en vez de
  imprimir un techo.
- `sameWallWithin = 1 h` — cuando la cuenta se seca, **cada agente escribe su
  propia línea de error** en segundos. Contarlas por separado triplicaría la
  muestra sin agregar una sola observación independiente, así que solo cuenta la
  primera de cada pared.
- **El techo viaja siempre como rango con su dispersión.** El % que imprime el
  comando cruza los extremos a propósito (*lo más consumido contra el techo más
  chico*), porque el costo de equivocarse es asimétrico: un agente que se para a
  media tarea cuesta más que uno que termina con cuota de sobra.

## 5. Peso por modelo: hoy dice `no derivable`, y eso es la respuesta correcta

**No sabemos cómo pondera Anthropic cada modelo contra la cuota de Max, y aquí no
se inventa.** El único camino honesto es derivarlo de lo observado: un modelo
pesa más si la ventana murió con **menos** de sus tokens, así que el peso es el
inverso del techo observado, normalizado contra el modelo más ligero.

Para que una ventana diga algo del peso de *un* modelo, ese modelo tiene que
haberla dominado (`dominantModelShare = 0.80`); si la ventana fue mezcla, no
separa una contribución de la otra. Y un modelo necesita dominar
`minObservationsPerModel = 3` ventanas antes de que valga la pena estimarle nada.

Con una muestra como la de §4, **un solo modelo** llega a ese piso
(`claude-opus-4-8`, 3 ventanas), y es el caso común. Y un peso es una **razón contra otro modelo** — la escala absoluta es
arbitraria —, así que un modelo solo, normalizado contra sí mismo, daría un
tautológico `×1.00` con cara de hallazgo. Por eso `minModelsToCompare = 2` retira
la cifra y el comando imprime la razón:

```
  Peso por modelo contra la cuota (Anthropic no lo publica; solo se deriva de lo observado)
    claude-opus-4-8  no derivable  es el único modelo con ventanas dominadas suficientes (3);
                                   un peso es una razón contra otro modelo y no hay con quién compararlo
```

Guarda adicional: `maxTrustedDispersion = 0.25`. Por arriba de ese coeficiente de
variación, los techos observados están tan dispersos que un peso ajustado sobre
los mismos datos sería **ruido con punto decimal**. En la muestra de §4 la
dispersión mide ±24%: ya pegada al límite de lo que esos datos aguantan.

**Consecuencia práctica:** el desglose por modelo del comando está en **tokens**,
no en "cuota ponderada". Los tokens son lo observable; la ponderación, no.

## 6. Ritmo y pronóstico

- **Ritmo** (`BurnRate`): tokens/h medidos sobre los últimos `burnLookback = 30
  min`, no sobre el promedio de la ventana. Un promedio desde que abrió la
  ventana sigue reportando la calma de la mañana a las 3 de la tarde.
- **Pronóstico** (`Project`): `restante_bajo / ritmo`. Usa el extremo pesimista de
  ambos rangos por la asimetría ya dicha. Si el ritmo no vacía la ventana antes
  del refill, lo dice con esas palabras — el caso sano merece nombrarse.
- Sin techo o sin ritmo **no hay pronóstico**, y el comando lo declara en vez de
  imprimir un número redondo.

Una lectura que parece inconsistente y no lo es: la ventana puede llevar 165
turnos mientras el ritmo se mide sobre 198. Las ventanas se **empalman de
corrido** bajo actividad continua, así que un lookback de 30 min puede caer a
caballo entre la cola de la ventana anterior y el arranque de la actual.

## 7. Cada proveedor con su propia forma de cuota

No se fuerza a todos a la forma de Anthropic; son objetos distintos, no el mismo
con números diferentes (`Plan` en `plan.go`):

| Proveedor | Ciclo(s) | Unidad | Techo | Confianza |
|---|---|---|---|---|
| Anthropic (Claude Max) | ventana rodante de 5 h · tope semanal | tokens | ventana: **calibrado** (§4). Semanal: **no publicado y nunca tocado aquí** → sin % | medido |
| Cursor | mes del plan (renueva el día que se configure) | **USD** | la mesada del plan, **publicada** → el % sí es un % real | actividad (rango) |
| Antigravity | — | — | no expone tokens ni cuota (bloqueo del lado de Google, `architecture.md` §4.2) | ninguna |

Antigravity aparece listado en `Sin cuota medible` **con su razón**. Un agente
ausente de la tabla se leería como "no consume", cuando la verdad es "nadie puede
saberlo".

El tope semanal se reporta con su consumo y su ritmo reales, pero **sin %**:
Anthropic no publica la mesada semanal y aquí nunca se ha tocado, así que no hay
techo del cual sacar porcentaje ni ancla de reset observada. La semana ISO se usa
como convención de reporte y se rotula como tal.

## 8. Turnos sin consumo legible ≠ turnos sin consumo

`Cycle.Unmeasured` cuenta los turnos dentro del ciclo cuyo consumo **en esa
unidad** no se pudo leer. Cursor lo hace obvio: turnos en el mes y `$0.00`
consumidos, sobre un plan que sí cuesta dinero real. Un `0%` ahí mide el medidor,
no el plan. Por eso el comando imprime:

```
    Consumido   $0.00 en 8 turnos (+8 turnos sin consumo legible: el real es mayor)
    Ventana     sin % calculable: ninguno de los 8 turnos del ciclo trae consumo legible;
                un 0% mediría el medidor, no el plan
```

Hay **tres** formas honestas de no tener porcentaje y el comando las distingue
(`noFillReason`): todos los turnos ilegibles · el techo no lo publica el
proveedor · la ventana aún no se puede calibrar.

## 9. Las palancas, y hasta dónde llega cada una

Las palancas se ordenan por cuánta cuota tocan (`lever.go`). Cada una lleva
evidencia medida y una **acción**: un reporte sin acción no mueve nada.

| id | Dispara cuando | Contrafactual |
|---|---|---|
| `P-01` concentración | un espacio de trabajo se lleva ≥ `concentrationShare = 40%` de la cuota, con ≥ `minTurnsForLever = 20` turnos detrás | topado a bajar ese espacio a la **mediana de los espacios** |
| `P-02` tokens/turno | un espacio corre a ≥ `heavyTurnRatio = 2.0×` la mediana de tokens/turno | `(tokens/turno − mediana) × turnos`, **topado a la mediana observada**, nunca a cero |
| `P-03` mezcla de modelos | el modelo dominante lleva ≥ `expensiveModelShare = 40%` y existe otro **realmente más ligero por turno** | **ninguno, a propósito** |

Tres detalles que costaron corridas equivocadas antes de quedar así:

- **`P-03` no reclama ahorro.** Convertir "31% menos tokens/turno" en "tanto de
  cuota ahorrada" exige justo la ponderación que §5 dice que no se puede derivar.
  La acción lo dice con todas sus letras: *"Cuánta cuota ahorra no se puede
  afirmar y no se inventa aquí"*, y agrega el otro caveat honesto: parte de la
  diferencia por turno es **el tipo de trabajo que recibe cada modelo**, no el
  modelo.
- **El candidato de `P-03` es el más ligero por turno, no el segundo por
  volumen.** Un modelo que carga el trabajo de contexto largo puede costar el
  doble de tokens/turno que el dominante aunque su precio de lista sea menor:
  recomendarlo *acortaría* la ventana.
- **Y tiene que cargar trabajo comparable** (`comparableTurnShare = 0.10` de los
  turnos del dominante): un modelo que solo hace tareas triviales siempre se ve
  barato. Sin ese piso, la primera corrida recomendaba un modelo con unas decenas
  de turnos a su nombre.

El tope de los contrafactuales viene de la misma lección que el plan por ruta
(`workload-classes.md` §5.1): sin él, la primera corrida reclamaba varias decenas
de veces el ahorro que sus datos podían sostener.

## 10. Verificación: recuento independiente contra el reporte

El comando tiene que reproducir lo que da un recuento independiente. La forma de
comprobarlo: recontar a mano `~/.claude/projects/**/*.jsonl` y
`~/.openclaw/agents/*/sessions/*.jsonl` con el mismo corte de días y comparar
contra el JSON del comando. Una corrida de referencia, para que se vea qué
diferencias son normales:

| Capa | Recuento independiente | Reporte del comando | Diferencia |
|---|---|---|---|
| Transcripts de Claude Code | 1,638.1M / 8,545 turnos | 1,637.9M / 8,525 turnos | −0.2M (**−0.01%**) / −20 turnos |
| Sesiones del gateway OpenClaw | 45.7M / 258 turnos con tokens | 45.7M / 257 turnos | 0 / −1 turno |
| Cursor | — | 6.0M / 7 turnos | — |
| Antigravity | — | 0.9M / 6 turnos | — |
| **Total flota** | — | **1,690.5M / 8,795 turnos** | — |

Las diferencias, explicadas y no maquilladas:

- **−20 turnos** en Claude Code: son exactamente los turnos de modelo
  `<synthetic>` con usage en cero. El adaptador los descarta; el recuento crudo
  los contaba.
- **−1 turno** en el gateway: compatible con los ~4 s de diferencia entre el
  corte inferior de una medición y otra (la flota estaba generando turnos
  mientras se medía).
- **Los dos recuentos por modelo cuadran exactamente:** el delta del reporte
  contra el recuento de Claude Code —+33.1M/159 turnos en `claude-sonnet-5` y
  +12.5M/99 en `claude-opus-5`— es, turno por turno, la capa del gateway de
  OpenClaw, que el glob de Python nunca tocaba.

**La tabla por espacio de trabajo suma menos que el total del periodo, a propósito.** Los turnos
del gateway de OpenClaw no traen directorio de trabajo (su formato no lo registra), así que
cuentan en *por agente* y *por modelo* pero no en *por espacio*: agruparlos en un cubo `""` los
haría verse como un lugar real. En la corrida de arriba son 45.7M de 1,692.4M.

Dos hallazgos de método que salieron de esta verificación:

1. **El análisis manual original subcontaba.** El glob `~/.claude/projects/*/*.jsonl`
   **no ve** `*/subagents/*.jsonl`; el `filepath.WalkDir` del adaptador sí. Todo
   el trabajo delegado a subagentes faltaba en la cifra hecha a mano.
2. **El espacio de trabajo es atributo de la sesión, no del turno.** El adaptador
   toma el `cwd` de la primera línea de la sesión y se lo aplica a toda ella. La
   divergencia más grande contra el recuento por turno lo muestra: un directorio
   puede salir 62.6M contando por turno y 0.5M contando por sesión, porque **62.1M**
   de esos tokens pertenecen a una sesión abierta en el `$HOME` que después se movió
   de directorio. Ninguna de las dos
   lecturas es falsa; la del comando responde *"¿en qué sesión se fue la cuota?"*,
   que es la pregunta que la palanca necesita.

Las cifras absolutas **no** van a coincidir entre dos mediciones tomadas con horas
de diferencia: la ventana se corre y la flota sigue trabajando. Lo que se reproduce
—y es lo que importa— son las **proporciones**: qué espacio de trabajo domina, a
cuántos tokens/turno, y qué modelo va por delante.

## 11. Lo que este comando NO dice

- **No dice cuánto dinero se gastó.** Ninguno de los agentes de Anthropic paga por
  token. El `$` de la última línea es equivalencia y lleva su rótulo pegado.
- **No dice cuánta cuota pesa cada modelo** (§5), ni cuánta ahorraría cambiar de
  modelo (`P-03`).
- **No aprende.** Sin estado entre corridas, sin modelo ajustado: mismos datos de
  entrada → mismo reporte.
- **No mide más capas.** No hay medición nueva aquí; toda la entrada son los
  mismos `aggregate.Record` que ya alimentaban `status` y `advise`.

## 12. Configuración

Los valores de negocio del plan son configuración con default documentado, no
constantes compiladas (`config.go`):

| Variable | Default | Qué es |
|---|---|---|
| `SPEND_CLAUDE_TIER` | `Max 5x` | Etiqueta del plan; solo se muestra (Anthropic no publica cifra por tier de la cual derivar nada). |
| `SPEND_CURSOR_MONTHLY_USD` | `200` | Mesada mensual del plan de Cursor. |
| `SPEND_CURSOR_RENEWAL_DAY` | `1` | Día del mes en que renueva. |
| `SPEND_CONTEXT_WARN_PCT` | `0.80` | A qué parte de la ventana de contexto empieza el aviso (acepta `0.80` u `80`). Por qué 0.80, medido, en `ventana-contexto.md` §5. |

Una variable puesta pero impresentable es **error**, no fallback silencioso: un
dedazo en el precio del plan cambiaría todos los porcentajes sin avisar.

## 13. Uso

```bash
llm-agent-spend-manager quota              # ventana actual + desglose de 3 días
llm-agent-spend-manager quota --days 7     # otro periodo para el desglose
llm-agent-spend-manager quota --days 0     # toda la historia
llm-agent-spend-manager quota --json       # mismo reporte, para máquinas
```

El desglose (`--days`) afecta la sección `QUIÉN SE LA COME` y **qué tan viejo**
puede ser el último turno de una conversación para que su contexto siga contando
como vivo. Los ciclos de `CUÁNTO QUEDA` son los que están en vuelo ahora mismo:
la ventana no se elige, se está adentro de ella.
