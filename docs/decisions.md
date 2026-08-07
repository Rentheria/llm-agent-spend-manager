# Decisiones de diseño (ADR)

Registro corto de las decisiones que explican **por qué el proyecto es como es**. Cada entrada
tiene contexto, la decisión, y el porqué — incluyendo lo que se descartó y a qué costo.

Estas decisiones son el resumen ejecutable de lo que ya está razonado a fondo en
[`architecture.md`](architecture.md) (el QUÉ del diseño), [`tech-stack.md`](tech-stack.md) (el
CÓMO: lenguaje y herramientas) y [`cuota.md`](cuota.md) (el método de medición). Cuando una
entrada de aquí y uno de esos docs se contradigan, mandan los docs largos: aquí va el resumen,
allá el detalle.

Regla de mantenimiento: un "descartado" sin razón anotada se vuelve un techo permanente que nadie
se atreve a tocar. Por eso cada alternativa descartada lleva su motivo, y revertir una decisión es
legítimo si el motivo dejó de aplicar — se anota el cambio y ya.

---

## ADR-01 — El núcleo es Go y no arrastra dependencias de terceros

**Contexto.** La herramienta es un CLI que cualquiera de la comunidad debe poder instalar sin
pelearse con un runtime. La primera elección fue Node/TypeScript, que es el lenguaje del resto del
stack de quien escribió esto.

**Decisión.** El núcleo (CLI, adaptadores, enforcement opcional) se escribe en Go, compilado a un
binario estático por sistema operativo, con el dashboard empotrado vía `embed`.

**Por qué.**
- **Instalación de un paso**: un binario, sin "primero instala Node/npm".
- **Superficie de ataque mínima**: la librería estándar cubre servidor HTTP y archivos empotrados,
  así que no hay árbol de dependencias npm que auditar. Los ataques de cadena de suministro en npm
  son un riesgo activo y documentado.
- **Transparencia**: un binario compilado desde un repo público es más fácil de verificar que una
  instalación con decenas de paquetes de terceros.
- **Cross-compile** a Linux/Mac/Windows desde una sola máquina.

**Costo asumido.** Go no es el lenguaje del resto del stack, así que el arranque fue más lento por
curva de aprendizaje. Se descartó Rust —mismos beneficios de binario nativo— porque la curva era
mucho más alta y no se justificaba para un proyecto de comunidad.

Detalle completo en [`tech-stack.md`](tech-stack.md).

---

## ADR-02 — No se reinventa un proxy multi-proveedor, ni se depende de uno

**Contexto.** Ya existen proxies multi-proveedor maduros con presupuesto por llave y dashboard
propio (tipo LiteLLM Proxy) que resuelven buena parte de este problema.

**Decisión.** No se usan como dependencia dura, y tampoco se compite con ellos. Lo que sí se
escribe es la pieza chica que falta: contar y topar, en unos cientos de líneas.

**Por qué.**
- Exigen levantar **un servicio más y su propia base de datos** solo para ver una gráfica —
  desproporcionado para el caso de uso.
- Ya traen su propio dashboard, así que montar otro encima sería redundante.
- La pieza que de verdad se necesita cabe en una fracción del tamaño, sin dependencias externas
  (coherente con ADR-01).

---

## ADR-03 — La visibilidad se apoya en huellas locales, nunca en la cooperación del fabricante

**Contexto.** Cada agente de la flota es de un fabricante distinto y ninguno expone una API de
"dime cuánto llevas". No existe un estándar para preguntarle a un agente cuánto gastó, y no se
puede exigir que cada CLI implemente algo a la medida.

**Decisión.** La herramienta **detecta huellas conocidas en el sistema de archivos** (directorios
de sesiones, bases SQLite locales) y se autoconfigura sola. Cada adaptador es un módulo aislado:
un agente nuevo se agrega sin tocar el resto.

**Por qué.** Lo único que todos los agentes tienen en común es que **dejan rastro local en
disco**. Diseñar contra eso es lo único que no depende de que un tercero decida cooperar. El
precio es que cada adaptador queda atado a un formato que su fabricante puede cambiar; a cambio,
la herramienta funciona hoy y sin pedirle permiso a nadie.

---

## ADR-04 — La unidad primaria es la ventana de cuota, no el `$`

**Contexto.** El reporte encabezaba con un `$`. Pero cuando los agentes corren sobre cuentas de
suscripción de precio fijo, ese número **no es dinero**: no hay cobro por token ni overage posible
por esa ruta.

**Decisión.** La cifra que encabeza es **cuánto de la ventana de cuota va consumido, a qué ritmo y
cuánto tiempo deja**. El `$` no se borra: baja a cifra secundaria y siempre viaja bajo su rótulo
obligatorio *costo equivalente estimado*, nunca *gasto real*.

**Por qué.** Lo que de verdad duele no es un cargo que nunca llega — es que **la ventana se acaba
antes de tiempo y los agentes se quedan a media tarea**. Esa es la restricción real, así que esa
es la que va al frente. Cada proveedor modela su cuota a su manera (ventana rodante en tokens,
mesada mensual en USD, o nada medible) y se representa así: aplanarlos a una sola forma sería
mentir sobre alguno.

Método completo en [`cuota.md`](cuota.md); el corolario normativo vive en
[`architecture.md`](architecture.md) §3.1.

---

## ADR-05 — El techo de la ventana se calibra desde agotamientos observados y viaja como rango

**Contexto.** El proveedor no publica el techo de la ventana de cuota, ni expone header, contador
ni endpoint para planes de suscripción. Lo único que expone, al negarse, es el reloj del refill.

**Decisión.** El techo se **calibra** desde las negativas reales que la propia máquina coleccionó,
y se reporta siempre como **rango con su dispersión**, no como cifra puntual. Cada instalación
calibra con las suyas; no hay constante compilada.

**Por qué.**
- La única verdad de campo disponible es *"en el instante T la cuenta ya estaba vacía"*. Todo lo
  demás sería inventado.
- Por debajo de un piso de observaciones, una mediana es **un solo dato disfrazado de
  estadística**: ahí el comando dice cuántas observaciones faltan en vez de imprimir un techo.
- Los porcentajes cruzan los extremos del rango a propósito, porque **el costo de equivocarse es
  asimétrico**: un agente que se para a media tarea cuesta más que uno que termina con cuota de
  sobra.

---

## ADR-06 — Tres niveles de confianza, y un dato nunca se promueve

**Contexto.** Unos agentes dejan tokens reales por turno en disco; otros solo dejan un registro
por conversación. Omitir a los segundos los mostraría como si no consumieran nada; ponerles una
cifra puntual los pondría a competir de tú a tú contra una medición real.

**Decisión.** Tres niveles explícitos y ordenados: **medido** (tokens reales del log) →
**costo equivalente estimado** (esos tokens × precio de lista) → **actividad estimada** (peso
relativo inferido de señales de actividad, reportado como rango, marcado con `≈`).

**Por qué.** La regla de oro es que **un dato medido nunca se degrada y uno estimado nunca se
promueve**. Sin esa jerarquía explícita, una estimación termina citada como medición dos pantallas
más abajo. Río abajo tampoco se relaja: donde se comparan rutas, las de actividad estimada
aparecen como *falta el dato* con su razón, en vez de entrar a la comparación con un `≈`.

---

## ADR-07 — Cuando un número no se puede derivar, se dice; no se rellena

**Contexto.** Varias cifras que sería cómodo imprimir no son derivables de lo observable: cuánta
cuota pesa cada modelo, cuánto ahorraría cambiar de modelo, o un porcentaje cuando ningún turno
del ciclo trae consumo legible.

**Decisión.** En esos casos la salida **imprime la razón en vez del número**, y distingue entre
las distintas formas de no tener el dato (todos los turnos ilegibles · el proveedor no publica el
techo · aún no hay con qué calibrar).

**Por qué.** Un `0%` donde el medidor no leyó nada **mide el medidor, no el plan** — y se lee como
un hecho tranquilizador. Un peso de modelo normalizado contra sí mismo daría un tautológico
`×1.00` con cara de hallazgo. Una cifra falsa con punto decimal hace más daño que un hueco
declarado, porque el hueco se puede llenar después y la cifra falsa ya se usó para decidir.

---

## ADR-08 — El enforcement es opcional, y su tope se cuenta en tokens reales sobre ventana deslizante

**Contexto.** Topar el gasto combinado no lo necesita todo el mundo, y las dos formas obvias de
implementarlo tienen fallos conocidos.

**Decisión.** El enforcement es una capa **opcional**, con un proxy propio y chiquito. El tope se
cuenta en los **tokens reales del `usage`** que el proveedor reporta en cada respuesta, sobre una
ventana **deslizante**.

**Por qué.**
- **Tokens reales, no bytes estimados**: estimar por bytes de la petición se intentó y no sirve —
  bajo prompt caching el estimado se va varias veces por encima de lo real. Además, los tokens son
  la misma unidad del techo del plan.
- **Ventana deslizante, no fija**: una ventana fija deja pasar ~2× el tope en la frontera (gastar
  el presupuesto entero justo antes del reinicio y otra vez justo después), que es exactamente el
  fallo que un tope existe para evitar.

---

## ADR-09 — El contador persiste en SQLite embebido por default; Redis es opt-in

**Contexto.** El tope necesita un contador que sobreviva reinicios y que coordine entre procesos.
El reflejo sería pedir Redis.

**Decisión.** Por default el contador vive en un archivo **SQLite** que la herramienta crea sola,
con el driver Go puro que ya está linkeado para los adaptadores. **Redis queda detrás de la
interfaz `enforce.Counter`**, activable por flag.

**Por qué.**
- El proxy corre como servicio: reinicia en cada actualización, crash y arranque. **Un tope que
  olvida al reiniciar es un tope que la flota puede rodear.**
- El driver ya está linkeado, así que persistir **no cuesta ni una instalación más** — instalar
  esto no requiere Docker ni Redis, basta el binario.
- El candado de escritura de SQLite es **entre procesos**, que era lo único por lo que se había
  considerado Redis en una sola máquina.
- Redis sigue siendo la respuesta correcta para el caso que sí lo exige: **varias instancias** del
  proxy compartiendo un tope. Ahí se reusa un algoritmo atómico ya probado (script Lua, agnóstico
  de lenguaje) en vez de reinventarlo o de escribir un daemon propio.

---

## ADR-10 — Default seguro: solo localhost; la red local es opt-in y con token

**Contexto.** El dashboard se quiere ver desde el celular, y el camino fácil sería escuchar en
todas las interfaces. El otro extremo sería exigir una VPN o malla privada para verlo.

**Decisión.** El servidor escucha **únicamente en `127.0.0.1`** por default. Exponerlo a la red
local es explícito (`--lan`) **y** obliga un token de acceso aleatorio, comparado en tiempo
constante, requerido en todas las rutas. El token va embebido en la URL y en el QR, así que el
celular sigue abriendo de un escaneo.

**Por qué.** Los dos extremos son malos: bindear a la red por default expone el historial de
actividad de la máquina a cualquiera en el WiFi, y exigir una VPN es fricción real que hace que
nadie lo use. Un opt-in con token cubre el caso de uso sin volver el default inseguro ni el camino
cómodo peligroso.

---

## ADR-11 — Nada obliga al usuario a cambiar cómo paga ni a mandar sus datos a la nube

**Contexto.** Dos funciones —el `$` real por token y el acceso remoto fuera de la red local— se
desbloquearían fácil si se exigiera BYOK (llave propia) y sincronización a la nube.

**Decisión.** Las dos son **opt-in**, nunca requisito. Sin BYOK, el adaptador correspondiente
sigue dando visibilidad de actividad out-of-the-box. Sin nube, el dato **nunca sale de la
máquina**.

**Por qué.** BYOK implica dejar una suscripción de precio fijo: es una decisión económica del
usuario, no una técnica que la herramienta pueda tomar por él. Y sincronizar a la nube implica que
el registro de actividad local sale hacia un tercero — un default razonable jamás hace eso sin que
lo pidan. Mismo principio en los dos casos: la herramienta funciona completa en la configuración
más privada.

---

## ADR-12 — Una sola base de UI web, envuelta para escritorio; sin app nativa por plataforma

**Contexto.** Hace falta llegar a terminal, escritorio y celular. Se propuso en serio un cliente
nativo multiplataforma (Flutter) y se consideró antes de descartarlo.

**Decisión.** El dashboard web servido por el núcleo es la **única base de UI**: terminal vía el
propio binario, escritorio vía un wrapper ligero (Tauri) sobre ese mismo dashboard, y celular como
PWA instalable desde el navegador.

**Por qué.**
- Meter un toolkit nativo encima serían **dos UIs distintas para los mismos datos**, duplicación
  real y permanente.
- La distribución de escritorio nativa tiene fricción propia: macOS pide notarización de paga o
  Gatekeeper avienta advertencias a cualquiera que lo baje — mala primera experiencia para
  adopción de un proyecto abierto.
- Las herramientas de referencia de esta categoría son CLI/terminal: la comunidad objetivo ya vive
  ahí, y un binario nativo para una tabla de gasto es sobre-ingeniería.

Queda como fase futura **solo si** el uso real demuestra que hace falta algo que la PWA no puede
dar (por ejemplo push nativo) — no como apuesta de entrada.

---

## ADR-13 — El 429 dice cuándo se libera la ventana real; el tope sigue anclado a la época

**Contexto.** El tope que aplica el proxy (`internal/enforce`) corre sobre una ventana anclada a la
época: `bucketIndex = now.UnixMilli() / window.Milliseconds()`. La ventana real de 5 h de Anthropic
no tiene esa fase: abre con el primer turno de la cuenta, al minuto que haya caído. El 2026-08-06 el
proxy rechazó a la flota con `combined budget cap reached` mientras la pantalla de Anthropic decía
"58% used, resets in 1h34min": el 429 no mentía sobre el tope propio, pero no decía nada cierto
sobre cuándo se podía volver a trabajar, y quien lo leía asumía lo segundo. `internal/quota` ya
reconstruye la fase real (`SessionWindows`, calibrada contra 5 agotamientos observados, error de 6 s
a 45.7 min) — pero solo alimentaba reportes y CLI, nunca la ruta que rechaza.

**Opciones consideradas.**
1. **Exponer el ETA real en el rechazo, sin tocar el mecanismo del tope** — el 429 y el aviso de
   Telegram dicen cuándo refila la ventana real; el tope sigue igual. Barato y reversible; no
   arregla que el tope corte en una fase distinta a la del plan.
2. **Anclar `windowState` al primer tráfico visto por clave**, persistiendo el inicio de ventana en
   SQLite junto al contador. Ataca la raíz aparente; cuesta migración del estado y **rompe la
   propiedad que el anclaje a la época compra**: hoy 4610 y 4611 coinciden en los mismos bordes sin
   coordinarse, y con un inicio persistido el borde pasa a ser un dato compartido más que puede
   quedar desincronizado.
3. **Calcular el ETA dentro del handler del 429**, sin caché. Siempre fresco; inviable: el escaneo
   de la máquina midió 11.5 s de reloj y ~110 MB de RSS pico (2026-08-06), y el guard que vigila
   este proxy le da 8 s al 429 completo antes de declararlo caído.
4. **Refrescar el ETA con un timer** cada N minutos. Siempre listo; gasta 11.5 s de CPU y un pico de
   ~110 MB para siempre, en una máquina compartida que ya se ha quedado sin memoria, para responder
   una pregunta que casi nunca se hace.

**Decisión.** La 1, con la lectura cacheada y refrescada **en segundo plano, disparada por el propio
rechazo** (`internal/sessionreset`). El proxy recibe una función que devuelve una frase ya calculada
(`proxy.WithResetNote`) y no sabe qué es una ventana de 5 h: la fase la reconstruye `internal/quota`,
que es donde está calibrada. Una ventana viva no se vuelve a escanear — su reset ya no se mueve —,
así que el costo se paga cuando la fase se desconoce o venció, no por reloj.

La 2 **no se implementa, y no solo por tamaño**: anclar al primer tráfico *visto por el proxy*
tampoco daría la fase real, porque el proxy solo ve lo que pasa por él, mientras que la ventana de
Anthropic la abre cualquier turno de la cuenta — incluidas sesiones interactivas que nunca lo
tocan. Se pagaría la coordinación entre carriles a cambio de una fase que seguiría siendo la
equivocada. La forma correcta de esa ruta sería alimentar el ancla desde la misma reconstrucción de
`internal/quota`, y eso ya es otro ticket.

**Consecuencias.**
- El tope sigue cortando en fase de época: **sigue siendo posible** que el proxy tope a la flota con
  quota real disponible, o que refile antes que el plan. Lo que cambia es que ahora el mensaje lo
  dice en lugar de callarlo. Si esa desalineación estorba en la práctica, el ticket siguiente es el
  ancla real, no este.
- El ETA puede venir de una lectura de hasta 5 minutos atrás cuando no hay ventana viva
  (`idleStaleAfter`), y el primer 429 tras arrancar el proxy puede llegar sin ETA si el escaneo
  inicial no terminó. Ambos casos se dicen con esas palabras; no se rellenan con un número.
- Si el escaneo falla, el 429 carga el error en vez de reportar "no hay ventana en vuelo", que se
  leería como buena noticia.
- El aviso de Telegram de `kazi-llm-lane-guard.sh` hereda la frase sin cambios de código: interpola
  el cuerpo del 429 (`$CUERPO`). Ese script vive fuera del repo.
- `humanize.Duration` sube de `cmd` a `internal/humanize` para que el 429 y el CLI deletreen la
  espera igual.

Refs: T139.
