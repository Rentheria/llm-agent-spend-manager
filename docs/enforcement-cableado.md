# Enforcement (Fase 3) — cableado por agente

> **Estado:** listo y **verificado en vivo** contra `api.anthropic.com`, con Redis y sin él. Se
> levanta con `llm-agent-spend-manager proxy`. Esta capa es **opcional**: solo entra si quieres un
> **tope duro combinado** entre agentes (ej. "todos los agentes juntos no pasan de X/día"). Todo lo
> demás de la herramienta funciona sin nada de esto. Ver `architecture.md` §3.1 y §3.2.

## Qué es y qué NO es

- **Es**: un proxy chiquito que reenvía la llamada real al proveedor y **topa** el uso
  combinado. Sin cuentas, sin UI propia, sin Postgres — justo lo que no se quiere duplicar
  de LiteLLM. **Sin dependencias externas**: el contador vive en un archivo SQLite que la
  herramienta crea sola.
- **No es**: una fuente de `$` real por defecto. Un agente sobre suscripción de precio fijo no
  genera cargo por token; el tope se cuenta en la unidad que elija el `AmountFunc` (requests,
  tokens, bytes de contexto). El único caso de `$` real por token es Cursor en **BYOK**.

## Piezas

- **`internal/enforce/window.go`** — el algoritmo: **ventana deslizante** de dos cubetas. La
  cubeta anterior se pondera por cuánto de ella sigue dentro del rango. Una ventana fija dejaría
  pasar ~2× el tope en la frontera.
- **`internal/enforce.Counter`** — dónde se lleva la cuenta. El default es `SQLiteCounter`
  (archivo propio, **cero instalación**, sobrevive reinicios, coordina entre procesos);
  `MemoryCounter` es `--state memory` para corridas desechables; `RedisCounter` es `--redis`, solo
  para un tope compartido **entre máquinas**.
- **`internal/enforce.Limiter`** — `Allow(key, amount) → Decision{Allowed,Current,Limit,Remaining}`.
  La **clave compartida** es lo que vuelve el tope *combinado*: si todos los agentes
  incrementan `fleet:default`, el tope aplica a la flota entera. Ventana rodante.
- **`internal/proxy.Proxy`** — `ServeHTTP` checa el tope y, si hay cupo, reenvía al `target`.
  Sobre el tope: `429` + `Retry-After`, sin tocar el upstream. Si el contador falla (solo
  alcanzable con Redis opt-in): **fail-open** (reenvía; marca `X-Cap-Enforced: fail-open`)
  para no tumbar a toda la flota.

## Cómo se levanta

    llm-agent-spend-manager proxy                       # cuenta, nunca bloquea (--cap 0)
    llm-agent-spend-manager proxy --cap 120000000 --window 5h   # topa en 120M tokens/5h

Flags: `--port` (4610) · `--target` (api.anthropic.com) · `--cap` (0 = solo cuenta) ·
`--window` (24h rodante) · `--unit` (`tokens` default | `context-bytes`|`requests`, estimadores) ·
`--key` (`fleet` combinado |
`agent` por header `X-Agent`) · `--state` (archivo del tope; `memory` para no tocar disco) ·
`--redis` (solo para topes entre máquinas).

**Empieza siempre con `--cap 0`**: confirma que el tráfico de todos los agentes realmente
pasa por el proxy *antes* de que un número pueda cortarle el trabajo a alguien a media tarea.

### Cableado permanente (systemd `--user`)

> La unidad de systemd, cómo instalarla (`deploy/systemd/`), el `loginctl enable-linger` que hace
> falta para que arranque en boot, y el servicio hermano del dashboard están en
> **`docs/servicios-permanentes.md`**.

    systemctl --user status lasm-proxy        # unidad: ~/.config/systemd/user/lasm-proxy.service
    systemctl --user restart lasm-proxy       # después de editar --cap en la unidad

`Restart=always` no es decoración: los agentes apuntan su `ANTHROPIC_BASE_URL` a este proceso, y
**proxy caído = flota sin LLM** (`connection refused`). El fail-open cubre que falle el *contador*,
no que falte el *proxy*. Por eso además va `enabled` (arranca al bootear) y con techos de recursos
(`MemoryMax=256M`, `CPUQuota=50%`) para no competir con los agentes en una máquina compartida.

Arrancar con **`--cap 0`** es lo recomendado: mide primero. Poner un número es una línea en el
`ExecStart` y un `restart` — no se prende solo por estar cableado.

### Cómo cablear los agentes — **un solo lugar**

Si varios agentes salen por el **mismo binario** de Claude Code, no hace falta cablear cada uno:
basta con `~/.claude/settings.json` → `"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:4610"}`.

Ése es el punto importante del diseño. Un runtime tipo OpenClaw que despacha contra
`cliBackends.claude-cli` no habla con el proveedor por su cuenta: lanza el mismo `claude`. Todos sus
modos (interactivo, cron/heartbeat, subagentes, headless) salen por ahí, así que ninguno tiene por
dónde escaparse del conteo. Definir en cambio un provider aparte en `openclaw.json` (patrón
`cloudflare-ai-gateway`) cablearía *un* camino y dejaría los otros a que alguien se acordara.

> ⚠️ **Un solo carril con tope te deja mudo a media sesión.** Si el mismo proxy que topa el trabajo
> desatendido sirve también la sesión interactiva, el primer `429` calla al humano que estaba
> esperando. La forma que funciona son **dos proxies contra el mismo `cap.db` y la misma
> `--key fleet`**:
>
> | carril | puerto | `--cap` | quién | ¿topa? |
> |---|---|---|---|---|
> | interactivo | 4611 | `0` | la sesión donde hay alguien esperando (default de `settings.json`) | **no** — cuenta y suma, nunca 429 |
> | desatendido | 4610 | el número real | workers y despacho automático (vía `--settings`) | **sí** |
>
> El consumo interactivo **sigue contando** en la misma cubeta (`enforce.Allow` incrementa y
> *después* decide), así que sigue empujando a los workers contra el tope; lo único que cambia es
> quién recibe el `429`. Regla: **el tope corta trabajo desatendido, jamás la sesión donde hay
> alguien esperando.**
>
> 📌 **Precedencia medida, no supuesta:** `~/.claude/settings.json` le **gana** a la variable de
> entorno `ANTHROPIC_BASE_URL`. Exportarla no cambia nada. Lo único que le gana al settings del
> usuario es la bandera **`--settings`** (archivo o JSON inline) — por eso el carril desatendido se
> cablea así y no con un `export`.
>
> 🔒 Conviene un guardián barato (un `.timer` cada pocos minutos, sin gastar tokens) que verifique
> que el carril sin tope sigue vivo, que el override apunta a él, y que el despacho desatendido no
> perdió su cableado al carril que sí topa. Si el carril interactivo se cae y nadie avisa, el
> settings manda las llamadas a un puerto muerto.

**Lo que NO cubre, dicho claro:** lo que no sea Anthropic. Un modelo local (Ollama) o el Gemini CLI
no pasan por aquí y no aparecen en este contador; Cursor tampoco (solo entraría en BYOK, §"Cursor").

**Revertir es borrar una llave:** quitar `env.ANTHROPIC_BASE_URL` de `settings.json` (guarda un
respaldo antes) y `systemctl --user disable --now lasm-proxy`.

Si prefieres cablear agente por agente:

### Claude Code
`ANTHROPIC_BASE_URL` (soporte oficial). Apuntar al proxy:

    export ANTHROPIC_BASE_URL="http://127.0.0.1:4610"

El proxy reenvía a `https://api.anthropic.com`. La autenticación (API key / credenciales del
CLI) viaja tal cual en los headers; el proxy no la toca, solo cuenta y topa.

### OpenClaw
OpenClaw soporta el **patrón de proxy-en-medio** por provider (el mismo formato que usa para el
gateway de Cloudflare). Se define un provider apuntando al `baseURL` del proxy local en
`~/.openclaw/openclaw.json`, cambiando la URL por `http://127.0.0.1:4610`. El agente no necesita
saber que hay un tope detrás.

> Nota de cobertura: un runtime así corre varios **modos** (interactivo, cron/heartbeat,
> subagentes, headless). Para que el tope sea de verdad combinado, **todos** esos modos deben salir
> por el mismo proxy — si algún modo llama al proveedor directo, se escapa del tope. Verificar al
> conectar en vivo. Por eso el cableado por `settings.json` de arriba es más robusto que declarar
> el provider.

### Cursor — solo BYOK, opt-in
Cursor soporta **"Override Base URL"** en modo **BYOK** (foro oficial). Es la **única** vía
por la que Cursor pasaría por el proxy, y **nunca es obligatoria**:
- **Sin BYOK** (default de la mayoría): Cursor sigue en su suscripción plana; el proyecto solo
  le da *visibilidad de actividad* vía `ai-tracking.db` (§4.1). No hay enforcement.
- **Con BYOK + Override Base URL** (opt-in): Cursor usa llave propia y sale por el proxy →
  desbloquea tope y `$` real por token, a cambio de dejar la tarifa plana de Cursor. Decisión
  del usuario, no técnica.

## Contexto por request — por qué esta capa NO es la primera palanca

Cuando el bucket dominante del reporte es `cache-read`, la pregunta obvia es dónde cablear
`internal/enforce` para atacarlo. El resultado hay que decirlo tal cual:

**`enforce.Limiter` no puede topar lo que la medición señaló.** Lo que topa es un **contador
rodante compartido** — un presupuesto de flota con ventana. Lo que encarece la cuenta es otra
cosa: **cuánto contexto arrastra cada sesión en cada turno**. Son preguntas distintas. Un tope de
presupuesto es un cortacircuitos: cuando la flota llega al número, deja de trabajar. No hace que
una sesión de 4,737 turnos sea más barata; la corta cuando ya se gastó.

El tope que sí ataca el problema medido es el de la **ventana de auto-compactación del runtime que
posee la sesión** (Claude Code, `autoCompactWindow`), porque ése sí actúa *dentro* de la sesión.
Por eso ése es el primer movimiento y este proxy no.

**Lo que sí se cableó aquí:** la *unidad* del tope. `OnePerRequest` (el default) le da el mismo peso
a un turno que arrastra 900k tokens de historia que a una pregunta de dos líneas — justo la
distinción que importa cuando el bucket dominante es `cache-read`. Ahora existe
**`proxy.ContextBytesAmount`**, que pesa cada request por los bytes de contexto que carga:

    px, _ := proxy.New("https://api.anthropic.com", lim,
        proxy.WithAmountFunc(proxy.ContextBytesAmount))   // el tope cuenta CONTEXTO, no llamadas

Así, si algún día se prende el tope combinado, se cuenta en la unidad que la medición dice que
manda. Bytes y no tokens estimados a propósito: el byte es exacto y no exige inventar una razón
tokens-por-byte (divide entre ~4 si quieres pensarlo en tokens).

Prenderlo sigue siendo decisión del operador por lo de siempre: para que el tope sea de verdad
combinado, **todos** los modos del runtime tienen que salir por el proxy (ver la nota de cobertura
arriba), y aplican las reglas de §"Seguridad al cablear" (loopback; y password al Redis si se usa
la opción `--redis`).

## Unidad del tope (`--unit`) — elegir con cuidado

El default es **`tokens`**: el proxy lee el `usage` que Anthropic
reporta en cada respuesta y cobra la suma de los cuatro buckets (`input + output +
cache_creation + cache_read`) — la misma unidad en la que está medido el techo del plan.
Es la "reconciliación posterior" que este documento listaba como futuro: ya está implementada
(`internal/proxy/usage.go`), y sirve igual en streaming (SSE) que en respuesta plana.

Las otras unidades siguen existiendo para no romper líneas de comando que ya andaban, pero son
**estimadores** y el banner lo dice al arrancar:
- **`context-bytes`**: pesa por los bytes de contexto del request (§"Contexto por request").
  Fue el default hasta que se midió contra el `usage` real y se rompió: la razón bytes/token
  fue **~17.5**, unas 7× arriba de la banda calibrada en sondas aisladas (2.2–3.14). La causa no
  es un factor mal medido sino que las dos magnitudes divergen justo donde vive la flota — cada
  turno reenvía el historial completo (los bytes crecen con el contexto) mientras el prompt
  caching abarata esos mismos tokens repetidos. Ninguna constante reconcilia eso.
- **`requests`** (`OnePerRequest`): tope por número de llamadas. Simple y exacto, pero una llamada
  gorda pesa igual que una chica.

Lo que hay que saber del modo `tokens`, porque cambia la semántica del corte:
- **El cargo llega DESPUÉS de la llamada** (es cuando existe el `usage`), así que lo que frena es
  la **siguiente**. El exceso está acotado por las llamadas en vuelo × tokens por llamada (unidades
  de millón contra un tope de 120 M); los headers `X-Cap-*` reflejan el contador **antes** del
  cargo de esa misma llamada.
- **La llave lleva la unidad adelante** (`tokens:fleet:default`, `scopeKeyByUnit`): cambiar
  `--unit` arranca ventana limpia en vez de heredar un número que medía otra cosa.
- **El proxy pide `Accept-Encoding: identity`** al upstream para poder leer el `usage`. Si aun así
  llega una respuesta comprimida, **no cuenta esa llamada y lo grita en el log** — nunca un cero
  silencioso.
- Una llamada billable (`POST …/messages`) que no traiga `usage` legible se reporta en el log en
  vez de pasar de noche; los health-checks (`HEAD /`) siguen sin contar ni hacer ruido.

## Trade-offs decididos (no re-litigar sin razón)

- **Ventana deslizante, no fija**: una ventana fija deja pasar ~2× el tope en la frontera. El
  costo es que la cubeta anterior se estima asumiendo tráfico parejo dentro de ella.
- **SQLite por default, Redis opt-in**: instalar la herramienta no puede exigir levantar una base
  de datos, y un tope que olvida al reiniciar es un tope que la flota puede rodear. Redis solo
  resuelve topes **entre máquinas**.
- **Fail-open** cuando el contador falla: disponibilidad > garantía dura. Un coordinador roto no
  debe bloquear a todos los agentes. Se marca con header para que sea auditable. Con el contador
  por default (archivo local) este camino casi no se alcanza: no hay servicio aparte que se caiga,
  solo el disco.
- **Cuenta-primero-luego-checa**: el incremento es lo que hace atómica la verificación
  concurrente; por eso un request sobre el tope sí aparece en `Current` aunque se rechace.
- **Solo el presente, no el histórico**: esta capa guarda la ventana vigente y nada más. El
  histórico de gasto/actividad ya lo dan los adaptadores.

## Verificado en vivo (con Redis) — canal Claude Code

> **Evidencia real, no supuesta.** Se probó el enforcement de punta a punta contra la API **real**
> de Anthropic, **solo en el canal de Claude Code** (`claude -p`). El canal de OpenClaw quedó
> explícitamente **fuera** de esta prueba. **No** se tocó `openclaw.json` ni el
> `ANTHROPIC_BASE_URL` de la sesión principal: la variable se pasó **inline solo al subproceso**
> `claude -p`.

**Montaje de la prueba (todo efímero, apagado al terminar):**
- Redis 7 **aislado** en `127.0.0.1:6399` (contenedor Docker propio, `redis:7-alpine`), para
  **no** interferir con el Redis de otro proyecto que ya corría en `6379`.
- Proxy de `internal/proxy` + `internal/enforce` escuchando en `127.0.0.1:4610`, reenviando a
  `https://api.anthropic.com`.
- **Tope combinado bajo a propósito:** `cap = 3000 tokens` sobre la clave compartida
  `fleet:default` (`DefaultKey`), ventana 24 h.
- **Unidad del tope (`AmountFunc`):** peso **plano de 2000 tokens** por llamada de completación
  (`POST …/messages`). Los health-checks de conexión que dispara Claude Code (`HEAD /`) se
  cuentan como **0** — de lo contrario el probe se comía el cupo antes del mensaje real. El peso
  plano es una estimación deliberada: `AmountFunc` corre **antes** de la respuesta, así que no
  ve el `usage` real (ver §"Unidad del tope"). Con 2000/llamada y tope 3000, la 1ª pasa
  (2000 ≤ 3000) y la 2ª se bloquea (4000 > 3000).
- Comando en ambas llamadas:
  `ANTHROPIC_BASE_URL=http://127.0.0.1:4610 claude -p "di solo la palabra ok" --dangerously-skip-permissions`

**Resultado — las tres condiciones se cumplieron:**

| # | Petición | Status | Contador Redis (`fleet:default`) | Qué probó |
|---|----------|--------|----------------------------------|-----------|
| Llamada 1 | `POST /v1/messages` (system prompt real, 246 706 bytes) | **200** | `0 → 2000` (remaining 1000) | (a) la llamada **pasó por el proxy** y llegó a Anthropic; `claude -p` devolvió **`ok`**, exit 0 |
| Llamada 2 | `POST /v1/messages` | **429** | `2000 → 4000` (remaining 0) | (b) se **descontó del tope** en Redis; (c) al superar el tope el proxy **bloqueó con 429** y **no tocó el upstream** (la 2ª llamada nunca llegó a Anthropic → **no facturada**) |

Log real del proxy (recortado a las líneas de las peticiones billables):

    17:09:45 --> IN  HEAD / ...                          (health-check)
    17:09:45 <-- OUT status=404 cap-current=0            (HEAD cuenta 0)
    17:09:48 --> IN  POST /v1/messages content-length=246706
    17:09:51 <-- OUT status=200 cap-current=2000 cap-remaining=1000 enforced=   ← LLAMADA 1: PASA
    17:10:09 --> IN  POST /v1/messages content-length=246706
    17:10:09 <-- OUT status=429 cap-current=4000 cap-remaining=0 enforced=      ← LLAMADA 2: BLOQUEA

El header `X-Cap-Enforced` salió **vacío** en el 429 (no `fail-open`): fue un rechazo por tope
real, no una degradación por Redis caído. La 2ª llamada fue **terminada** por reintentos sobre
el `Retry-After: 60` — evidencia de que Claude Code no pudo completar porque el proxy la frenó.

**Notas / limitaciones honestas de esta prueba:**
- El tope se contó en **peso plano por llamada**, no en tokens reales de la respuesta (eso
  exigiría la reconciliación posterior descrita en §"Unidad del tope" — no la había entonces;
  hoy es el default). La prueba valida el **mecanismo** (pasa → cuenta en Redis → bloquea 429), no la exactitud del
  estimador de tokens.
- Un request real de Claude Code carga un system prompt **grande** (~247 KB ≈ decenas de miles
  de tokens por `bytes/4`); un tope realista en tokens reales sería mucho mayor que el de esta
  prueba forzada.
- **Al terminar se apagó y eliminó todo** (proxy + contenedor Redis de prueba): por defecto no
  queda nada corriendo en background. Cablearlo de forma permanente es una decisión aparte
  (§"Cómo se levanta").

## Verificado en vivo — **sin Redis**, solo el binario

Se repitió la prueba **después de parar y borrar el contenedor `lasm-cap-redis`** y con el puerto
6399 libre: nada de Docker, nada de Redis, ninguna variable de entorno.

| # | Montaje | Petición | Status | Contador | Qué probó |
|---|---|---|---|---|---|
| A | `proxy --port 4613 --cap 3000` | `POST /v1/messages` (1,892 bytes) | **401** | `0 → 1,892` | La llamada bajo el tope **sí llegó a Anthropic** (401 = llave falsa, o sea el upstream respondió) |
| B | idem | misma petición | **429** | `1,892 → 3,784` | Sobre el tope **bloqueó sin tocar upstream** |
| C | idem | tercera | **429** | — | El bloqueo persiste dentro de la ventana |
| D | `proxy --port 4614 --cap 0` | `claude -p "Responde exactamente: ok"` **real** | **ok** | `→ 254,575` context-bytes | Tráfico real de Claude Code atraviesa el proxy y se contabiliza, en modo solo-cuenta |
| E | `proxy --port 4615 --cap 3000 --state ./cap.db`, **matado y relevantado** entre peticiones | `POST /v1/messages` | **429** | leído del archivo | El tope **sobrevive al reinicio del proxy** — justo lo que el contador en memoria no podía hacer |

Las filas A-D corrieron con el contador en memoria (`--state memory`); la fila E con el default
de hoy (archivo SQLite), que es la que justifica el cambio de default.

Comportamiento **idéntico** al de la corrida con Redis. Es la prueba del requisito de producto: *quien instale esto no tiene que instalar nada más*. Al terminar se apagó el proxy y se
liberó el puerto (higiene de recursos).

## Verificado en vivo — **cableado permanente, tráfico real de la flota**

Ya no es una prueba con un proxy efímero: es el servicio `lasm-proxy` corriendo, con el
`settings.json` apuntándole.

| # | Qué se probó | Cómo | Resultado |
|---|---|---|---|
| 1 | El servicio levanta y solo escucha loopback | `ss -ltnp` | `LISTEN 127.0.0.1:4610`, nada en `0.0.0.0` |
| 2 | Una llamada real atraviesa y se cuenta | `claude -p "Responde exactamente: ok"` | respondió **`ok`**, contador `0 → 248,464` context-bytes |
| 3 | El cableado vive en `settings.json`, no en la variable inline | misma llamada con `env -u ANTHROPIC_BASE_URL` | **`ok`**, `+248,464` — lo agarró del settings |
| 4 | **Los modos del runtime desatendido también salen por aquí** | mismo binario con un **entorno pelón** (`env -i HOME=… PATH=…`, sin `ANTHROPIC_BASE_URL`) | **`ok`**, `+248,467` — no depende de heredar el entorno de un shell |
| 5 | El servicio se repone y el tope no se olvida | `kill -9` al proceso | `NRestarts=1`, `active` a los ~2 s, contador **intacto** (1,473,133) |
| 6 | La sesión interactiva viva está pasando por el proxy | leer el contador sin hacer llamadas de prueba | siguió subiendo solo (`1,473,133 → 1,972,086`) al ritmo de los turnos reales |

La fila 4 es la que cierra el hueco de cobertura: no se probó "el runtime funciona", se probó que
**el camino por el que arranca `claude` no tiene manera de saltarse el proxy**, porque el binario
lee el `settings.json` aunque lo lancen sin entorno.

## Seguridad al cablear (obligatorio)

Reglas duras para esta capa, derivadas de una auditoría de seguridad del proyecto: **exponer un
servicio a red no confiable sin auth es inaceptable**. El comando `proxy` las hace cumplir en código: el listen
address se construye siempre sobre `127.0.0.1` (el `--port` no admite host), y `--redis` rechaza
al arrancar cualquier dirección que no sea loopback o que venga sin `SPEND_REDIS_PASSWORD`.

- **Proxy y Redis SIEMPRE en loopback (`127.0.0.1`), NUNCA `0.0.0.0`.** El proxy reenvía las
  credenciales upstream del usuario (`Authorization`/`x-api-key`) sin tocarlas; si se bindea a
  la LAN, cualquier peer podría enrutar por él y **gastar con las llaves del usuario**. Si algún
  día el proxy debe cruzar red, esto **sube a HIGH** y exige autenticación propia (mismo patrón
  que `serve --lan`: token aleatorio, comparación en tiempo constante) antes de cablearlo.
- **Password/ACL al Redis aunque sea local**, si se usa la opción `--redis`. El tope viviría en
  Redis; sin auth, cualquier proceso local puede vaciar la clave y desactivar el límite. Poner
  `requirepass` (o ACL) y pasarlo por `SPEND_REDIS_PASSWORD` — **nunca en `argv`**, donde
  cualquier usuario local lo lee con `ps`. `buildCounter` se niega a arrancar sin esa variable.
- **Fail-open está documentado y es intencional** (`proxy.go:110-116`): con Redis caído el proxy
  reenvía y marca `X-Cap-Enforced: fail-open`, para no tumbar a toda la flota. El trade-off
  aceptado es que **tumbar o comprometer el Redis local desactiva el tope en silencio** — por eso
  el Redis va en loopback + con password. Si el caso de uso exige garantía dura del tope, cambiar
  a fail-closed es una decisión explícita, a registrar donde vivan las decisiones del proyecto.
- **El proxy no loguea credenciales** (verificado: sin sentencias de log de
  headers de auth). Mantenerlo así: nunca loguear `Authorization`/`x-api-key`.
