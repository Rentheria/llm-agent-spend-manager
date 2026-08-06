# Arquitectura y diseño

Doc de diseño, no de código. Objetivo: **ver, optimizar y opcionalmente topar** el gasto de LLM
de una flota de agentes heterogéneos (Claude Code, OpenClaw, Cursor, Antigravity, y los que
vengan) desde una sola herramienta que no exige cambiarle nada a ninguno de ellos.

El stack tecnológico (lenguaje, frameworks, por qué se descartó cada alternativa) vive en
[`docs/tech-stack.md`](tech-stack.md). Este doc es el QUÉ y el CÓMO a nivel de diseño.

## 1. Problema real

Varios agentes corriendo en paralelo gastan tokens cada uno por su cuenta, sin que exista una
vista unificada de *"cuánto gastó cada quien, hoy/esta semana, en qué tarea"* — ni forma de topar
el gasto combinado si se dispara. Cada agente es de un fabricante distinto y ninguno expone una
API de "dime cuánto llevas": lo único común es que **todos dejan rastro local en disco**.

## 2. Qué NO se reinventa

- **Proxies multi-proveedor con presupuesto por llave y dashboard propio** (tipo LiteLLM Proxy)
  ya resuelven ese problema de forma madura. **No se usan como dependencia dura**, por dos
  razones: (a) exigen levantar un servicio más y su propia base de datos solo para ver una
  gráfica, y (b) ya traen su propio dashboard, así que montar otro encima sería redundante. La
  pieza que sí se necesita —contar y topar— cabe en unos cientos de líneas (§3.2).
- **Un protocolo para "preguntarle a un agente cuánto gastó"** no existe como estándar, y no se
  puede exigir que cada CLI implemente algo a la medida. Por eso el diseño se apoya en **huellas
  locales** (§3.1) y nunca en la cooperación del fabricante.

## 3. Componentes del sistema

### 3.1 Capa de visibilidad — adaptadores de auto-detección ("huellas")

En vez de pedirle a cada agente que se describa, la herramienta **detecta huellas conocidas en el
sistema de archivos** y se autoconfigura sin intervención manual. Cada adaptador es un módulo
aislado: si aparece un agente nuevo, se agrega un adaptador sin tocar el resto.

| Agente | Huella a detectar | Fuente de datos |
|---|---|---|
| Claude Code | `~/.claude/projects/*/`, sesiones JSONL | Logs locales por sesión con **tokens reales** por turno → **costo equivalente estimado**. |
| OpenClaw | `~/.openclaw/` (sesiones JSONL + `openclaw.sqlite`) | Sesiones con **tokens reales** por turno → **costo equivalente estimado**. El `usage.cost` que expone el log sale **0** (suscripción / CLI sin billing por token), así que el `$` se calcula desde los tokens. El SQLite aporta además el uso de **cron/heartbeat**, que no aparece en las sesiones. |
| Cursor | `~/.cursor/ai-tracking/ai-code-tracking.db` (SQLite) | Modelo usado, conversación, timestamp, atribución de código IA/humano. **No trae tokens ni `$`** — solo actividad (§3.3). Para `$` real: BYOK (§4.1) + enforcement (§3.2). |
| Antigravity | `~/.gemini/antigravity-cli/conversation_summaries.db` (SQLite) | Metadata de conversación (agente, pasos, timestamps). **No trae tokens ni `$`**, y hoy no hay forma de conseguirlo del lado del proveedor (§4.2). |

> **Nota importante sobre el `$` (leer antes de mirar cualquier número de dólares):** los agentes
> cubiertos corren típicamente sobre **suscripciones de precio fijo**, **no** sobre facturación
> por token de la API. Los **tokens sí son reales** (salen del log de cada agente), pero el `$`
> que calcula este proyecto es un **costo equivalente estimado**: `tokens reales × precio público
> de lista de la API`, *como si* se pagara por token. Sirve para (a) comparar el peso relativo
> entre agentes/tareas/modelos y (b) como proxy de qué tan cerca está uno del tope de uso de la
> suscripción — **pero NO es dinero cobrado**. El dashboard y el CLI lo rotulan siempre como
> *"costo equivalente estimado"*, **nunca** *"gasto real"*, para que no se lea como un cargo
> sobre un plan de tarifa plana. (El único caso donde el `$` sí sería cobro real por token es
> Cursor en modo **BYOK** con llave propia — §4.1 —, que es opt-in.)

> **Corolario normativo: la unidad primaria es la CUOTA, no el `$`.** La nota de arriba explica
> que el `$` no es dinero; lo que faltaba era decir qué sí lo es. Lo que de verdad se acaba —y
> detiene a los agentes a media tarea— es la **ventana de cuota del proveedor**: en Claude Max,
> una ventana rodante de 5 h más un tope semanal, ambos con techo no publicado. Por eso la cifra
> que encabeza es **cuánto de la ventana va consumido, a qué ritmo y cuánto tiempo deja**, y el
> `$` queda como cifra secundaria bajo su rótulo. Cada proveedor mide su cuota a su manera y se
> modela así (Anthropic en tokens contra un techo calibrado; Cursor en USD contra una mesada
> publicada; Antigravity no expone cuota y se reporta como no medible) — aplanarlos a una sola
> forma sería mentir sobre alguno. El techo de Anthropic **no se inventa**: se calibra desde los
> agotamientos observados en la máquina y viaja siempre como rango con su dispersión; cuando los
> datos no alcanzan, la salida lo dice en vez de fabricar un número. Método completo en
> [`docs/cuota.md`](cuota.md).

### 3.2 Capa de enforcement (opcional, no todos la necesitan)

Solo entra en juego si se quiere un **tope duro combinado** entre agentes ("todos juntos no
pasan de X tokens por ventana"). Componentes:

- Un **proxy propio, chiquito**: reenvía la llamada real y topa el uso combinado antes de
  reenviar. Nada de cuentas, UI ni base de datos externa. Se levanta con
  `llm-agent-spend-manager proxy`.
- **El tope se cuenta en tokens reales del `usage`** que el proveedor reporta en cada respuesta
  (streaming incluido): `input + output + cache_creation + cache_read`. Es la misma unidad del
  techo del plan. Estimar por bytes de la petición se intentó y no sirve: bajo prompt caching el
  estimado se va varias veces por encima de lo real.
- **El tope es una ventana DESLIZANTE**, no fija (`internal/enforce/window.go`): dos cubetas por
  clave, la anterior ponderada por cuánto de ella sigue dentro del rango. Una ventana fija deja
  pasar ~2× el tope en la frontera —gastar el presupuesto entero justo antes del reinicio y otra
  vez justo después—, que es exactamente el fallo que un tope existe para evitar.
- **El contador va por default en un archivo SQLite**
  (`~/.local/state/llm-agent-spend-manager/cap.db`). El proxy corre como servicio, o sea reinicia
  en cada actualización, crash y arranque: un tope que olvida al reiniciar es un tope que la
  flota puede rodear. El driver ya está linkeado para los adaptadores (Go puro, sin CGO), así que
  persistir **no cuesta ni una instalación**, y el candado de escritura de SQLite es **entre
  procesos**, así que dos proxies en la misma máquina comparten un tope correctamente.
  **Instalar esto no requiere Docker ni Redis: basta el binario.** `--state memory` para corridas
  desechables.
- **Redis es opt-in** (`--redis`), detrás de la misma interfaz `enforce.Counter`, para el único
  caso que sí lo necesita: **varias instancias** del proxy compartiendo un tope. Ahí reusa un
  algoritmo atómico ya probado (script Lua, agnóstico de lenguaje — ver
  [`tech-stack.md`](tech-stack.md)), que es el escenario para el que ese algoritmo se escribió: N
  servidores que no se conocen entre sí.

**Viabilidad por agente:**

- **Claude Code**: soporta `ANTHROPIC_BASE_URL` (documentado por el fabricante). ✅
- **OpenClaw**: ya soporta el patrón de proxy-en-medio a nivel de provider. ✅
- **Cursor**: soporta "Override Base URL" en modo BYOK. ✅, con el trade-off de §4.1.
- **Antigravity**: ❌ bloqueado del lado del proveedor — ver §4.2.

El cableado concreto está en [`docs/enforcement-cableado.md`](enforcement-cableado.md).

### 3.3 El nivel de actividad estimada (cuando no hay tokens que leer)

Cursor y Antigravity **no dejan tokens por turno en disco**: su rastro local es un registro por
**conversación** (modelo, pasos, timestamps, texto). Hay dos maneras de tratar eso y solo una es
honesta:

- **Omitirlos** haría que la tabla los muestre como si no consumieran nada. Un agente ausente se
  lee como un agente gratis.
- **Ponerles una cifra puntual** los pondría a competir de tú a tú contra una medición, que es
  justo lo que la jerarquía de confianza existe para impedir.

Por eso existe un **tercer nivel, explícitamente por debajo de lo medido**:

1. **Medido** — tokens reales del log.
2. **Costo equivalente estimado** — esos tokens reales × precio de lista (§3.1).
3. **Actividad estimada** — peso relativo inferido de señales de actividad (largo del texto de la
   conversación y señal de tracking de código en Cursor; conteo de `step` en Antigravity),
   reportado como **rango, no punto**, marcado con `≈`, y **sin `$`** cuando el modelo de la
   conversación no es legible confiablemente.

**La regla de oro:** un dato medido nunca se degrada al nivel de uno estimado, y uno estimado
nunca se promueve. Río abajo la jerarquía tampoco se relaja: en el plan por ruta
([`workload-classes.md`](workload-classes.md)) las rutas de actividad estimada aparecen como
*falta el dato* con su razón, en vez de entrar a la comparación con un `≈`.

**Cobertura, y su hueco conocido:** un agente puede gastar por rutas que su log de sesiones no
registra — el caso típico es el trabajo automático de fondo (**cron/heartbeat**), que en OpenClaw
vive en `openclaw.sqlite` y no en los `.jsonl`. El adaptador lee las dos fuentes y las deduplica;
sin eso, el total del agente saldría corto y nadie lo notaría. Cuando una ruta de gasto no se
puede leer, se **nombra** como no cubierta en vez de omitirse.

## 4. Principios de diseño

### 4.1 BYOK es opcional, nunca obligatorio

El adaptador de Cursor funciona igual de bien para **cualquier usuario**, use o no BYOK:

- **Sin BYOK** (default, mayoría de usuarios): visibilidad de actividad vía `ai-tracking.db` —
  funciona out-of-the-box, sin pedirle a nadie que cambie su forma de pagar Cursor.
- **Con BYOK + Override Base URL** (opt-in): además desbloquea `$` real y enforcement vía el
  proxy (§3.2), a cambio de dejar la suscripción plana de Cursor — decisión del usuario, no
  técnica.

### 4.2 Plan incremental para Antigravity (bloqueado hoy, no descartado)

Antigravity no admite endpoint/llave propia hoy (issue abierto sin resolver del lado del
proveedor: `google-antigravity/antigravity-cli#514`). Es un bloqueo del proveedor, no algo
arreglable desde aquí ni con un plugin de editor. Aun así, tres cosas de bajo esfuerzo valen la
pena:

1. **Visibilidad parcial ya disponible**: `step_count` + timestamps por conversación — sin `$`,
   pero mejor que nada (§3.3).
2. **Sumar señal en el issue oficial**, que es el canal legítimo.
3. **Adaptador diseñado para activarse solo**: si el proveedor agrega soporte de endpoint propio,
   se prende con un flag, sin rediseñar.

Sobre parsear los archivos protobuf internos (`brain/`, `implicit/*.pb`): se descartó por ser un
formato no documentado que se rompe con cualquier actualización, y después **se revirtió el
descarte** — el protobuf sí se decodifica por wire-format y da un piso medido de tokens por
generación. El descarte era razonable cuando se escribió; el dato resultó valer el esfuerzo. Se
deja anotado porque un "descartado" sin fecha de revisión se vuelve un techo permanente.

**Este bloqueo es de MEDICIÓN, no de DESPACHO.** Lo que Antigravity no permite es apuntar sus
llamadas a un endpoint propio, que es lo que haría falta para *contarle* el gasto. Mandarle
trabajo funciona perfecto (corre headless y sale con rc=0). Por eso puede ser motor de respaldo
aunque su gasto siga siendo invisible para este proyecto — son dos capacidades distintas y
confundirlas cuesta un respaldo que sí existía.

### 4.3 Acceso mobile sin fricción, no vía VPN por defecto

Exigir instalar y configurar una VPN o una malla privada solo para ver el dashboard es fricción
real para cualquier usuario.

- **Default seguro — solo localhost:** `serve` escucha únicamente en `127.0.0.1`; nada se expone
  a la red hasta que el usuario lo pide con `--lan`.
- **Opt-in a la LAN con token:** `serve --lan` bindea la red local (`0.0.0.0`) **y** exige un
  token de acceso aleatorio (128 bits, `crypto/rand`), comparado en tiempo constante, requerido
  en todas las rutas. El token se imprime en el banner y va incluido en la URL/QR, así que el
  celular sigue abriendo de un escaneo.
- **Sin ni siquiera teclear la IP:** con `--lan --qr` el CLI imprime un código QR (con el token
  embebido) que apunta directo al dashboard.
- **Acceso remoto (fuera de la red local)**: opcional y avanzado, sin exigir ninguna herramienta
  específica — quien lo quiera usa la que ya tenga.
- **Sincronización a la nube**: sería una opción válida de fase futura para acceso remoto fácil y
  push real, pero implica que el dato sale de la máquina local. Decisión explícita del usuario,
  nunca el camino por defecto (mismo principio que BYOK).

### 4.4 Respaldos cuando la cuota se agota

El tope del proxy (§3.2) evita **llegar** al techo. Esto cubre lo otro: qué corre cuando el techo
ya se topó. La regla que ordena todo es que **un respaldo solo sirve si corre sobre una cuota
distinta a la del titular** — si comparte la cuota, no es respaldo, es el mismo muro con otro
nombre. Anthropic, Cursor y Google son tres cuotas independientes.

Para **trabajo headless** eso se cablea encadenando motores: si el primero truena, corre el
siguiente. Dos detalles que cuestan sangre si se omiten:

- La señal de "tronó" no puede ser solo la firma de error conocida: un proceso que muere
  escribiendo 15 bytes y `rc≠0` también tronó, y sin esa segunda señal el fallback no dispara.
- Cada motor debe mirar **solo lo que él escribió** (por offset en el log). Si el log es
  acumulativo y no se acota, el tercer motor hereda la firma de error del primero y se da por
  muerto antes de empezar.

Para una **sesión interactiva** no hay relevo transparente, y conviene decirlo en vez de
descubrirlo en la emergencia: la persona/configuración del agente no viaja como system prompt
entre runtimes distintos, y el historial de la conversación vive del lado del proveedor que se
quedó sin cuota. Un suplente que finge acordarse de los últimos turnos miente sobre algo
verificable. Lo honesto es un **standby que se invoca** y que se presenta como tal.

**Observabilidad de los cortes del proxy.** El `ErrorHandler` por defecto de
`httputil.ReverseProxy` solo sabe decir el error, y con eso no se distingue si el proxy causó el
corte o si fue la primera víctima de un cliente que ya se estaba muriendo. Por eso el proxy lleva
`ErrorHandler` propio y anota, en la misma línea de log: `first_cut` (si el contexto del cliente
ya estaba cancelado cuando tronó el forward = el cliente se fue primero; si seguía vivo = cortó
el upstream), `class`, `ctx_err`, `elapsed`, `client_gone_after`, `resp_status` y `client_bytes`.
También registra el caso que antes no dejaba rastro: cuando la respuesta ya arrancó y se corta a
medio stream, `ReverseProxy` hace `panic(http.ErrAbortHandler)` sin llamar al `ErrorHandler` y
net/http se lo traga en silencio. Es **solo observabilidad**: cero cambio de comportamiento, sin
reintentos ni timeouts nuevos, porque remediar sin saber la causa es justo el error que la
escalación a brecha de arquitectura existe para evitar
([`automejora.md`](automejora.md)).

## 5. Alcance

**Núcleo:** la capa de visibilidad (§3.1) para los agentes que ya dan **tokens reales → costo
equivalente estimado** sin configuración adicional, más el razonamiento sobre lo medido
(cuota, automejora, forma de la carga, bitácora de resultado). Dashboard web + CLI.

**Opcional / fase futura:**

- Enforcement (§3.2): proxy con tope combinado, sin dependencias externas.
- Cursor en modo BYOK (§4.1).
- Adaptadores de actividad-solamente para más agentes (§3.3).
- Acceso remoto fuera de la LAN (§4.3).
