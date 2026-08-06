> 🌐 **Español** (este archivo) · [**English**](README.en.md)

# llm-agent-spend-manager

Visibilidad y control de gasto de LLM entre agentes (Claude Code, OpenClaw, Cursor,
Antigravity, y más) — un solo binario Go, sin dependencias npm, sin servicios de terceros
obligatorios.

**Estado: funcional y usable hoy.** Cubre 4 agentes con jerarquía de confianza visible,
desglose por modo de uso, terminal + dashboard web (LAN/celular). Reporta en la unidad que de
verdad se acaba —**cuánto queda de la ventana de cuota**, no cuántos dólares equivalentes—. Y
además de **medir**,
razona sobre lo medido: sabe cuándo un consejo suyo ya dejó de servir, qué forma tiene cada
carga de trabajo, y si una métrica cambió de nivel de verdad o si el movimiento cabe en el
ruido. No es un plan a futuro: `go build` y ya reporta datos reales de tu máquina.

## La regla de la casa: no se inventa ningún número

**Todo lo que el reporte afirma se deriva de datos medidos.** Si algo no se puede derivar, el
binario lo dice — `n/a`, `falta el dato`, `insufficient-data`, `sin clasificar` — en vez de
rellenarlo con una estimación que se leería igual que una medición. No hay modelo ajustado, no
hay librería de ML, no hay estado guardado entre corridas: mismos datos de entrada → mismo
reporte.

Eso suena a buena intención hasta que cuesta cifras. Tres lugares donde muerde de verdad y el
resultado queda **más chico y más honesto**:

- **El contrafactual topado a lo observado.** El plan por ruta podría decir que mover *todos* los
  turnos de una forma al modelo más barato habría ahorrado una fortuna — aunque ese modelo solo
  se haya observado cargando una fracción de esos turnos. Eso es una extrapolación disfrazada de
  aritmética. Con el tope, solo se reclama el ahorro sobre los turnos que la opción barata **ya
  demostró** cargar, y el reporte **dice en voz alta que topó y por qué**. Método:
  [`docs/workload-classes.md` §5.1](docs/workload-classes.md).
- **Una caída de dos dígitos que no se reclama como mejora.** En la bitácora de resultado,
  `cost-per-turn` puede bajar un 38% y aun así el veredicto ser **`sin cambio de nivel`**: si el
  escalón mide menos de 1σ contra la dispersión diaria de la propia serie, cabe en el ruido. Un
  reporte que titulara "bajamos 38%" estaría vendiendo varianza como resultado. Método:
  [`docs/bitacora-resultado.md` §2](docs/bitacora-resultado.md).
- **Un peso por modelo que se retira en vez de publicarse.** Anthropic no publica cómo pondera
  cada modelo contra la cuota de Max, así que solo se deriva de lo observado — y con frecuencia
  un solo modelo llega al piso de evidencia. Un peso es una **razón contra otro modelo**, así que
  ese único candidato daría `×1.00`: una tautología con cara de hallazgo. El comando imprime
  **`no derivable` con el motivo**. Método: [`docs/cuota.md` §5](docs/cuota.md).

La misma regla es la que hace que `SavingsUSD` sea siempre un **tope y no una promesa**, y la
que obliga a `TemporalCaveat` en cada atribución: coincidencia temporal no es causalidad.

- Diseño: [`docs/architecture.md`](docs/architecture.md)
- Stack tecnológico y por qué: [`docs/tech-stack.md`](docs/tech-stack.md)
- Método de la cuota (ventana, techo calibrado, palancas): [`docs/cuota.md`](docs/cuota.md)
- Método del reporte de automejora: [`docs/automejora.md`](docs/automejora.md)
- Forma de la carga y plan por ruta: [`docs/workload-classes.md`](docs/workload-classes.md)
- Bitácora de resultado: [`docs/bitacora-resultado.md`](docs/bitacora-resultado.md)
- Enforcement opcional (topes): [`docs/enforcement-cableado.md`](docs/enforcement-cableado.md)
- Servicios permanentes (systemd de usuario): [`docs/servicios-permanentes.md`](docs/servicios-permanentes.md)

## Agentes cubiertos

Todos los agentes cubiertos corren sobre suscripción de precio fijo, así que **nada de lo que
reporta es "gasto real"**. Hay una **jerarquía de confianza** explícita, de más a menos exacta:

1. **Medido — costo equivalente estimado.** Tokens reales del log × precio de lista de la API.
   - **Claude Code** — parseo de los transcripts JSONL locales; tokens exactos por turno.
   - **OpenClaw** — parseo de las sesiones JSONL; incluye también el gasto de
     **cron/heartbeat** (leído de `openclaw.sqlite`), que de otro modo no aparecería.
2. **Actividad estimada.** El agente no expone tokens ni `$`; se infiere peso relativo desde su
   actividad y se reporta como **rango (no punto)**, marcado con `≈` y un escalón por debajo de
   los medidos.
   - **Cursor** — actividad estimada a partir del texto real de las conversaciones + señal de
     tracking de código.
   - **Antigravity** — actividad estimada por conteo de steps; **sin precio** (modelo por
     conversación no legible confiable) → reporta solo tokens/actividad, nunca `$`.

Jerarquía visual, de mayor a menor confianza: **medido > costo equivalente estimado >
actividad estimada**. El dashboard y el `status` distinguen los tiers (borde sólido + cifra
única para lo medido; borde punteado + badge `≈` + rango para actividad estimada).

La jerarquía **no se relaja río abajo**: en el plan por ruta, Cursor y Antigravity aparecen
listados como *falta el dato* con su razón, en vez de meter un `≈` a competir contra una cifra
medida.

## Desglose por modo

Además del total por agente, `status` y `/api/summary` desglosan el uso por **modo**:

- **interactivo (chat)** — turnos de conversación,
- **cron / heartbeat** — trabajo automático de fondo,
- **editor** — asistencia de código (Cursor / Antigravity).

## Uso

```bash
go build -o llm-agent-spend-manager ./cmd/llm-agent-spend-manager

./llm-agent-spend-manager status               # tabla de texto (hoy) — --window today|week|all
./llm-agent-spend-manager quota                # qué queda de la ventana, quién se la come y qué la estira
./llm-agent-spend-manager advise               # dónde se va el costo, la forma de cada carga y qué cambiar
./llm-agent-spend-manager outcome              # qué cambios reales se hicieron y si la métrica se movió
./llm-agent-spend-manager serve                # dashboard SOLO en localhost (http://localhost:4600)
./llm-agent-spend-manager serve --lan          # expón a la red local (0.0.0.0) + token de acceso
./llm-agent-spend-manager serve --lan --qr     # + QR (con token) para abrirlo desde el celular
```

**¿Dejarlo prendido siempre?** Un medidor que hay que acordarse de prender solo mide los ratos en
que alguien se acordó. Hay unidades de systemd **de usuario** listas en `deploy/systemd/` (sin
`sudo`, sin root) para el dashboard y para el proxy de conteo: ver **`docs/servicios-permanentes.md`**.

`status` (una foto del gasto de hoy):

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

> ⚠️ **Toda la salida pegada en este README es un EJEMPLO ILUSTRATIVO**, con cifras inventadas y
> redondeadas para que se lea el formato. No son mediciones de nadie. Lo que veas al correr los
> comandos sale de tu propia máquina y no se parecerá a esto.

**Por defecto, `serve` escucha solo en `127.0.0.1`**: nada queda expuesto a la red hasta que
lo pidas con `--lan`. Al usar `--lan` el binario genera un **token de acceso aleatorio**
(128 bits) y lo exige en **todas** las rutas (dashboard y `/api/*`); sin token responde `401`.
El token se imprime en el banner y va **incluido en la URL y el QR**, así que desde el celular
sigue abriendo de un solo escaneo. Puedes fijar el token con `--token <valor>` o la variable de
entorno `LASM_TOKEN` (útil para reusar el mismo token entre reinicios). **Prefiere `LASM_TOKEN`:**
lo que pasas por `--token` queda en el `argv` del proceso y cualquier usuario de la máquina lo ve
con un `ps`.

Flags de `serve`:

| Flag | Default | Para qué |
|---|---|---|
| `--port <n>` | `4600` | Puerto TCP donde escuchar. |
| `--lan` | apagado | Bindea la red local (`0.0.0.0`) **y** exige token en todas las rutas. |
| `--token <valor>` | aleatorio (128 bits) | Fija el token en vez de generarlo. Solo aplica con `--lan`. |
| `--qr` | apagado | Imprime un QR de la URL de LAN (con token). Requiere `--lan`. |
| `--cache-ttl <dur>` | `10s` | Cuánto se reusa un escaneo antes de volver a leer disco. `0` desactiva el caché. |
| `--local` | apagado | **Obsoleto**, no-op (ver abajo). |

> `--local` sigue aceptándose pero está **obsoleto** (loopback ya es el default); es un no-op
> con aviso de deprecación, para no romper invocaciones viejas.

El dashboard es instalable como PWA desde el navegador del celular. Hay un wrapper de
escritorio (Tauri) en [`desktop/`](desktop/).

## La unidad que sí duele: la ventana de cuota, no el `$`

Si tu flota corre sobre **suscripción de precio fijo**, el `$` nunca fue el recurso escaso: es
equivalencia. Lo que de verdad se acaba —y para a los agentes a media tarea— es la **ventana de
cuota del proveedor**. Una ventana de 5 h de Claude Max se puede agotar en 2–3 h si varios
agentes trabajan en paralelo, y los eventos de `rate_limit` son la única señal dura de que pasó.

Por eso `quota` encabeza con la cuota y deja el `$` abajo, con su rótulo. Contesta tres
preguntas en ese orden: **cuánto queda · quién se la come · qué palanca la estira más.**

```
$ ./llm-agent-spend-manager quota --days 3

CUÁNTO QUEDA

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

  Peso por modelo contra la cuota (Anthropic no lo publica; solo se deriva de lo observado)
    claude-opus-4-8  no derivable  es el único modelo con ventanas dominadas suficientes (3);
                                   un peso es una razón contra otro modelo y no hay con quién
                                   compararlo
```

Cada proveedor tiene **su propia forma de cuota** y así se modela: Anthropic en tokens contra un
techo calibrado (ventana rodante + tope semanal), Cursor en **USD** contra una mesada publicada
—el único caso donde el `$` sí es dinero—, y Antigravity sin ciclo, listado como **no medible con
su razón**, porque un agente ausente de la tabla se leería como "no consume".

Y quién se la come sale solo, sin que nadie escarbe (recortado):

```
QUIÉN SE LA COME
  Total del periodo: 300.0M tokens en 2,000 turnos · $200.00 costo equivalente estimado

  Por espacio de trabajo (separa la plática del trabajo de código)
                            TOKENS   %      TURNOS  TOKENS/TURNO
    .openclaw/workspace     150.0M   50.0%  1,000   150,000   OpenClaw
    Develop/mi-proyecto     90.0M    30.0%  700     128,571   Claude Code
    Develop/otro-repo       30.0M    10.0%  200     150,000   Claude Code

QUÉ PALANCA LA ESTIRA MÁS

  [P-01] .openclaw/workspace se lleva 50% de la cuota
      Evidencia: OpenClaw · 150.0M de 300.0M tokens en 1,000 turnos, a 150,000
                 tokens/turno (la mediana de los espacios es 128,571)
      Acción:    Aquí es donde se decide la ventana, no en los repos de código. Si
                 .openclaw/workspace no es trabajo de código, la palanca es cortar el contexto
                 que arrastra: sesiones más cortas, resumen en vez de historial completo, o
                 mover ese tráfico a un modelo más ligero.
```

**El hallazgo que el comando tiene que gritar solo**: cuando la mitad de la cuota se va en un
espacio de trabajo que no es código —una conversación larga, en el modelo más caro, a cientos de
miles de tokens por turno— eso sale en el primer renglón, no repartido en migajas de un dígito.

Dos cosas que este comando **se niega** a decir, y son parte del punto:

- **Cuánto pesa cada modelo contra la cuota.** Anthropic no lo publica. Solo se deriva de
  ventanas que un modelo haya dominado (≥80%, mínimo 3), y como un peso es una *razón* contra
  otro modelo, con un solo modelo calificado se imprime `no derivable` **con el motivo** en vez
  de un `×1.00` que sería una tautología con cara de hallazgo.
- **Cuánta cuota ahorraría cambiar de modelo.** La palanca `P-03` sí nombra un modelo más ligero
  por turno cuando lo hay, pero **no reclama ahorro**: cuantificarlo exigiría justo la
  ponderación que no se pudo derivar, y parte de la diferencia por turno es el tipo de trabajo
  que recibe cada modelo, no el modelo.

El total se puede verificar contra un recuento independiente de los transcripts crudos; el
método, la calibración, los márgenes y cómo hacer esa verificación están en
[`docs/cuota.md`](docs/cuota.md).

## De medir a razonar sobre lo medido

Un total no es una palanca. Las cinco secciones siguientes son **una sola cadena**: medir dónde
se va el costo → saber cuándo el consejo que se dio ya dejó de servir → medir la forma del
contexto para poder dar uno mejor → clasificar la forma de cada carga y comparar la ruta que la
corrió → y por último contrastar los cambios que de verdad se hicieron contra la métrica.
Cada eslabón existe porque el anterior se quedó corto.

### 1. Medir dónde se va el costo, no cuántos tokens se gastaron

Un token no vale lo que otro token: al mismo modelo un token de `output` cuesta ~50x uno de
`cache-read`. Sumar tokens apunta a la palanca equivocada, así que `advise` atribuye el costo
equivalente a los cuatro buckets facturables y los ordena **por costo**.

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

Sobre eso corren los **hallazgos con id estable (E-01…E-08)**, cada uno con su evidencia
numérica para que se verifique en vez de creerse. Si no hay nada que reportar, se calla. La
métrica de eficiencia es **costo por turno**, no total: trabajar más sube el total y eso no es
una regresión. Método completo en [`docs/automejora.md`](docs/automejora.md).

### 2. Cuándo un consejo dejó de ser un consejo

Un reporte que solo sabe dar tips empeora cierta clase de problema: cada vez que el mismo
hallazgo reincide, su costo tocado sube, y un sistema ingenuo lo promueve **con más fuerza justo
cuando la evidencia dice que el consejo es el remedio equivocado**.

Por eso `advise` clasifica el fallo antes de elegir el remedio. Un hallazgo que se emitió **3
ventanas seguidas** (de 3 días activos) sin que su métrica mejorara **sale de la lista de tips
por completo** y aparece como **brecha de arquitectura**, nombrando el fierro que falta:

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

*(ejemplo ilustrativo, con los saltos de línea reacomodados para que quepa)*

Esto **no** contradice la regla de "la herramienta mide, no aprende": no hay bitácora de qué
consejos se emitieron. La reincidencia se establece **re-ejecutando las mismas reglas** sobre las
ventanas anteriores del mismo conjunto de records, lo que hace la afirmación más fuerte —
*la condición lleva N ventanas siendo cierta*, corriera alguien el reporte o no. El mecanismo
sugerido sale de una tabla fija indexada por id. Detalle en
[`docs/automejora.md` §5](docs/automejora.md).

**No escala sin historia suficiente:** con menos de 9 días activos la sección no aparece, y con
`--window today` nunca vas a ver escalaciones.

### 3. La curva de contexto y el punto de no-retorno

Para dar un consejo mejor que "haz sesiones más cortas" hacía falta medir algo que no se estaba
midiendo: no *cuánto costó* una sesión, sino **cuánto contexto cargaba en cada turno**. Es la
pregunta que importa porque `cache-read` — el bucket más caro en la mayoría de las flotas — escala
con **(tamaño del contexto × turnos)**.

La forma real es un serrucho: el contexto sube ~1,000 tokens por turno hasta el techo de la
ventana, algo lo compacta, cae a una base chica, y vuelve a subir. Dos números lo describen
(`Baseline`, `GrowthPerTurn`), ambos **medianas** para que una corrida desbocada no fije ninguno.

El **punto de no-retorno** es el turno donde el sobreprecio acumulado de arrastrar el contexto
(`cache-read`, cada turno) supera lo que cuesta re-armar el prefijo desde cero (`cache-write`,
una vez). Se resuelve en forma cerrada, así que describe **la forma** de la sesión y no depende
de hasta dónde llegó a correr:

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

*(ejemplo ilustrativo; en la salida real hay además una columna `QUÉ SE PIDIÓ` con el texto de la
petición que abrió cada sesión)*

Dos cosas que esta medición hace bien y conviene saber:

- **Agrupa por (sesión, hilo), no por sesión.** Claude Code escribe los turnos de sus subagentes
  bajo el **mismo `sessionId`**, y cada subagente lleva su propio contexto. Una sesión larga puede
  ser miles de turnos de hilo principal más miles de subagentes bajo un solo id; mezclarlos hace
  que cada relevo parezca un reinicio y la curva no describe nada.
- **El ahorro es un tope, no una promesa.** La cuenta es exacta *en tokens* — se pagaron de
  verdad — pero cortar una conversación también tira lo que ya sabía, y lo que cueste volver a
  leer esos archivos **no está medido**. La advertencia va pegada a la cifra, no escondida en un
  doc.

De aquí sale un valor de configuración concreto: **bajar la ventana de auto-compactación** del
runtime que corre las sesiones (por ejemplo de 1,000,000 a 200,000 tokens). Es configuración del
runtime, no de esta herramienta: el reporte dice cuánto se pagaría de más sin ese tope, y quien
opera la máquina decide si lo aplica.

### 4. La forma de cada carga y el plan por ruta (Capas 2 y 3)

Dos sesiones que costaron lo mismo no son el mismo problema. Una que arrastró contexto creciente
cuatro mil turnos se abarata cortándola; una que murió en su primer turno tras escribir una caché
que nadie leyó se abarata **no pidiendo la caché**. Capa 2 clasifica cada carga (un *hilo de
contexto*, la misma unidad de la §3) en una de cuatro formas, con reglas explícitas y la palanca
que le corresponde:

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

Lo que no casa se reporta **`sin clasificar` con la razón**; nunca se redondea a la forma más
cercana, porque "casi una ráfaga mecánica" es un dato inventado. Es normal que una sola forma
—conversación larga— concentre casi todo el costo; ése es justamente el titular que el comando
debe entregar sin que nadie escarbe.

Capa 3 compara, para cada forma, lo que cobró **cada ruta que la corrió** — y es donde el tope de
observación de la §"regla de la casa" hace su trabajo:

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

*(ejemplo ilustrativo: una de las cuatro formas, con los renglones largos plegados)*

Tres reglas la mantienen honesta: solo rutas que **corrieron** esa forma (una ruta sin
observación es *falta el dato*, nunca se interpola); solo rutas **medidas**; y **misma forma ≠
mismo entregable** — nada aquí verifica que la ruta barata hubiera entregado lo mismo, así que
toda cifra es un tope y una hipótesis a probar, no una orden de mover el trabajo. Un modelo local
a costo $0 se **excluye** del contrafactual y se dice que se excluyó: cuesta $0 por correr en
hardware propio, no por ser más eficiente. Umbrales y su derivación en
[`docs/workload-classes.md`](docs/workload-classes.md).

### 5. La bitácora de resultado: consejo → cambio real → ¿se movió? (Capa 4)

La §2 sabe decir *"esto no está mejorando"*. Lo que no sabe es *"esto mejoró **después de** aquel
cambio"*, porque nunca supo qué cambios se hicieron. `outcome` aporta esa mitad: lee los cambios
que **de verdad ocurrieron** (commits de git, sin merges, + los eventos `commit`/`done`/`fix` del
log de la flota) y los contrasta contra las series diarias de las métricas.

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

Ese primer renglón es la regla de la casa cobrando: **−38.7% y aun así `sin cambio de nivel`**,
porque −0.9σ contra la dispersión de la propia serie no distingue un escalón del ruido diario.

Cómo funciona, en dos pasos que se rehacen con una calculadora: **dónde** (CUSUM — el extremo de
la desviación acumulada marca dónde se juntan los dos regímenes) y **cuánto** (las dos medias
contra su **desviación estándar agrupada**, con veredicto en `|Δ| > 1.0σ`). Hay cuatro
veredictos, y `insufficient-sample` **es un resultado, no un fallo**.

La atribución no adivina: los cambios candidatos son los que caen entre el día del escalón y 2
días **activos** hacia atrás; si hay varios, **se listan todos y no se le acredita a ninguno**; si
no hay ninguno, eso es un hallazgo real (lo que lo movió no está en esta bitácora). Cada
atribución carga el aviso obligatorio de que coincidencia temporal no es causalidad.

Y la parte que más cuesta escribir — lo que **no** se puede calificar sale listado con su razón:

```
  SIN SERIE DIARIA (no se puede calificar, y por eso no se califica)
        E-02 · métrica wasted-cache-cost-share
        E-07 (escalado: ya había dejado de ser un tip) · métrica past-no-return-context-cost-share
        Por qué: esta métrica se define por sesión, y una sesión partida a medianoche no da un valor
        diario honesto — sin serie diaria no hay nivel que comparar.
```

Hoy **solo E-01 tiene serie diaria**, y es una correspondencia exacta: su métrica *es* el share
del costo equivalente de un bucket. Agregar otra métrica es una función por métrica, siempre que
tenga un valor diario defendible. Método en
[`docs/bitacora-resultado.md`](docs/bitacora-resultado.md).

`outcome` lee git y el log de la flota, que son I/O de **afuera** de los datos de uso — por eso es
un comando aparte y no una sección de `advise` (`advise.Analyze` es función pura de los registros
de uso, y un commit no es un registro de uso). Rutas configurables con `--repos` y `--log`.

## Dónde sale cada cosa

No todo está en todas las superficies, y conviene saberlo antes de buscarlo:

| Sección | Terminal | `--json` | HTTP | Dashboard |
|---|:--:|:--:|:--:|:--:|
| Totales por agente y por modo | `status` | ✅ | `/api/summary`, `/api/daily` | ✅ |
| Ventana de cuota, ritmo y pronóstico | `quota` | ✅ | ❌ | ❌ |
| Quién se come la cuota y qué palanca la estira | `quota` | ✅ | ❌ | ❌ |
| Buckets facturables y tendencia | `advise` | ✅ | `/api/advice` | ✅ |
| Tareas más caras | `advise` | ✅ (`topTasks`) | `/api/advice` | ❌ |
| Contexto pasado del punto de no-retorno | `advise` | ✅ | `/api/advice` | ✅ |
| Hallazgos y brechas de arquitectura | `advise` | ✅ | `/api/advice` | ✅ |
| Forma de la carga y plan por ruta | `advise` | ✅ (`workloads`) | `/api/advice` | ❌ |
| Bitácora de resultado | `outcome` | ✅ | ❌ | ❌ |

```bash
./llm-agent-spend-manager advise  --window all --json   # para que lo consuma otro agente
./llm-agent-spend-manager outcome --window all --json
curl localhost:4600/api/advice                          # y por HTTP, igual que el resto de la API
```

## Lo que NO hace

Para que el estado no se lea inflado:

- **No hay ML, ni modelo, ni aprendizaje.** Todo es derivación determinista sobre datos medidos:
  mismos records → mismo reporte, sin estado guardado entre corridas. La bitácora de resultado es
  precisamente el **prerrequisito** de cualquier modelo futuro — es la que produciría el dataset
  `(cambio, métrica antes, métrica después)` — no un sustituto de uno.
- **`outcome` no tiene ruta HTTP ni panel en el dashboard.** Es CLI + `--json`. El dashboard hoy
  renderiza lo que expone `/api/advice`, y `outcome` no pasa por ahí.
- **`quota` tampoco está en el dashboard, y es a propósito.** El dashboard sigue encabezando con
  el `$`; pintarle la cuota encima antes de que la unidad estuviera bien habría sido rehacer el
  panel dos veces. Es CLI + `--json` hasta que se retome el dashboard.
- **El techo de la cuota de Anthropic no está publicado por nadie.** El que reporta `quota` es un
  **estimado calibrado** con los agotamientos que hayan ocurrido en tu máquina, con su rango y su
  dispersión — es un rango con margen ancho, no una medición. Por debajo de 3 agotamientos
  observados el comando no imprime techo alguno.
- **La sección de forma de carga y plan por ruta tampoco está en el dashboard.** Sale en la
  terminal y bajo la llave `workloads` del JSON de `advise`.
- **Solo E-01 tiene serie diaria.** E-02 y E-07 se definen **por sesión**, y una sesión partida a
  medianoche no da un valor diario honesto: el número sería un artefacto del corte. Se listan
  como `SIN SERIE DIARIA` con su razón en vez de calificarse mal.
- **La forma de carga de Cursor y Antigravity es inmedible hoy.** Exponen un registro por
  conversación, no por turno: no hay turnos, ni buckets, ni curva. Ése es el hueco más grande —
  la palanca "rutéalo a un agente barato" no se puede cuantificar con los datos que hay, y el
  reporte lo dice en vez de rellenarlo.
- **El tope de contexto es una recomendación, no algo que esta herramienta aplique.**
- **Ningún ahorro reportado es una promesa.** Todos son topes sobre tokens observados; ninguno
  mide lo que cuesta perder el contexto ni verifica que la ruta barata hubiera entregado lo mismo.

## Enforcement (opcional, apagado por defecto)

Existe una capa de **topes duros** combinados entre agentes: un proxy chiquito por el que salen
los agentes, que cuenta y bloquea al pasarse. **Está apagada por default** — el MVP de
visibilidad funciona sin ella y solo entra si se cablea a propósito:

```bash
llm-agent-spend-manager proxy                              # cuenta, nunca bloquea
llm-agent-spend-manager proxy --cap 120000000 --window 5h  # topa en 120M tokens/5h
```

El tope se cuenta en **tokens reales**: el proxy lee el `usage` que el proveedor reporta en cada
respuesta (streaming incluido) y suma `input + output + cache_creation + cache_read`, que es la
misma unidad del techo del plan. Nada de estimar por bytes — se intentó, y bajo prompt caching el
estimado se fue ~7×.

**No necesitas instalar nada más** — ni Docker, ni Redis, ni un servicio: el contador vive en un
archivo SQLite que la herramienta crea sola y que sobrevive a los reinicios. El tope se cuenta con
una **ventana deslizante**, no fija, así que no se puede colar el doble del presupuesto a caballo
de la frontera. (`--state memory` para no tocar disco; `--redis` para compartir un tope entre
máquinas distintas.) El proxy escucha **solo en loopback** y reenvía
tus credenciales del proveedor sin tocarlas; por eso nunca se bindea a la red. Cómo apuntar cada
agente está en [`docs/enforcement-cableado.md`](docs/enforcement-cableado.md).

Vale la pena leerlo junto con la §2: las brechas de arquitectura que el reporte escala nombran
exactamente esa clase de fierro — un tope que no dependa de que alguien se acuerde.

## Desarrollo

```bash
go build ./...
go vet ./...
go test -p 1 ./...    # -p 1: un proceso de test a la vez (máquina compartida)
```

Requiere **Go 1.26.5+** (es lo que fija `go.mod`, y lo que usa el CI). El mínimo subió de 1.25
por seguridad: el stdlib de Go anterior traía vulnerabilidades alcanzables desde este binario.
Binario único, sin dependencias npm ni servicios de terceros obligatorios.

**Ramas:** el trabajo activo va a **`dev`**; **`main`** es estable y se mergea desde `dev`
cuando está listo — nunca se commitea directo a `main`. Ver [`CONTRIBUTING.md`](CONTRIBUTING.md).

**Próximo candidato (no urgente):** adaptador para **Codex CLI** de OpenAI. Guarda logs
locales JSONL con tokens reales por turno igual que Claude Code, así que sería tier **medido**
sin estimación.
