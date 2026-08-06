# Archivos de arranque: el impuesto que paga cada sesión

> **Qué es esto:** la lista de archivos compartidos que **cada agente lee al abrir
> sesión**, cuánto pesan hoy, y el umbral a partir del cual la herramienta avisa
> (hallazgo `E-08`). No es gasto de un proveedor: son bytes en disco, medidos con
> `os.Stat`, sin ninguna llamada a API de por medio.
>
> **Regla que se respeta aquí:** no se inventa ningún número. Cada umbral de este
> documento sale de una medición real de esta máquina, con la fecha en que se tomó,
> y se declara explícitamente como **punto de partida a recalibrar**, no como
> límite validado. Ver `docs/calibracion.md` para la misma regla aplicada a los
> factores de estimación.

---

## Por qué se mide

Un archivo que **todos** los agentes cargan en **cada** boot se paga otra vez en
cada sesión, por cada agente, para siempre — toque o no toque la tarea de hoy algo
de su contenido. Tiene la misma forma que las métricas de costo de
`internal/advise`: un costo fijo, recurrente, que nadie factura y que nadie nota.

El disparador fue real. El **2026-07-31**, `~/.openclaw/workspace/AGENTS.md` ya
documentaba un patrón de **índice + detalle** para tareas cerradas:
`state.json.tasks_cerradas.ids` guarda solo `id` + `title`, y la nota completa vive
en `tasks-cerradas.json`. Pero **31 tareas cerradas** habían conservado su nota
completa pegada al índice de arranque. Migrarlas a mano bajó `state.json` de
**69 KB a 33 KB**.

El patrón existía. Nadie lo estaba vigilando, así que se rompió en silencio. Este
documento y el paquete `internal/bootfiles` son solo la métrica que avisa la
próxima vez.

**Fuera de alcance a propósito:** si un archivo *debería* contener lo que contiene
es un juicio humano. Aquí se mide **tamaño en bytes contra un umbral**, nada más.

---

## Archivos vigilados y sus umbrales

La lista es deliberadamente corta: cada entrada está aquí porque una sesión real la
lee al arrancar, no porque pareciera pertenecer a una lista genérica de archivos
dignos de vigilancia.

| Archivo | Baseline medido | Umbral (2×) | Origen del baseline |
|---|---:|---:|---|
| `~/.openclaw/workspace/state.json` | 33 KB (33 792 B) | **67 584 B** | Tamaño **post-limpieza** del 2026-07-31: el índice con solo lo que `AGENTS.md` dice que debe tener. **Es un known-good.** |
| `~/.openclaw/workspace/SYNC.md` | 64 KB (65 536 B) | **131 072 B** | Primera medición, 2026-07-31 (62.9 KB, redondeado a 64 KB). **NO es un known-good** — ver abajo. |

### La regla del 2×, y su único ancla

El umbral es **el doble del baseline documentado**. Tiene exactamente **un ancla
real**: la erosión que sí se detectó tenía `state.json` en 69 KB contra un índice
limpio de 33 KB — **2.1×**. O sea, duplicar es el tamaño al que la deriva ya fue
visible y cara una vez en esta máquina, en lugar de un número escogido por redondo.

**Un dato es un dato.** Esto es un punto de partida, no un límite validado. Lo
honesto es escribirlo, decir de dónde salió, y ajustarlo cuando haya más corridas
contra las cuales calibrar.

### La honestidad sobre `SYNC.md`

`state.json` tiene un baseline confiable porque se midió justo después de limpiarlo
contra un patrón documentado. **`SYNC.md` no**: nunca se ha limpiado, así que su
baseline **hornea la deriva que ya trae**. Sirve como piso para detectar crecimiento
adicional, y para nada más. Si algún día se limpia con un criterio explícito, ese
tamaño post-limpieza es el que debe reemplazar el baseline aquí.

---

## Cómo se ve la sección

Ejemplo ilustrativo de la salida de `llm-agent-spend-manager advise` (rutas y
tamaños inventados; lo que veas sale de tus propios archivos):

```
ARCHIVOS DE ARRANQUE (los lee cada agente en cada sesión)
  ARCHIVO                                         TAMAÑO   UMBRAL    CAMBIO                       ESTADO
  /home/user/.openclaw/workspace/state.json  34.7 KB  66.0 KB   sin cambio desde 2026-07-31  dentro (margen 31.3 KB)
  /home/user/.openclaw/workspace/SYNC.md     63.3 KB  128.0 KB  sin cambio desde 2026-07-31  dentro (margen 64.7 KB)
  El umbral es 2x una línea base medida, no un límite validado: punto de partida
  a recalibrar con más corridas (ver docs/archivos-arranque.md).
```

Cuando los dos están **por debajo** de su umbral, la corrida **no levanta el
hallazgo `E-08`**. Ese es el resultado correcto: la métrica se entrega verde porque
el estado del disco está verde, no porque se haya ajustado el umbral para que lo
esté.

Para ver el aviso end-to-end sin tocar los umbrales reales, se puede forzar por entorno
(`SPEND_BOOT_FILES=".../state.json=1024,.../SYNC.md=131072"`):

```
  /home/user/.openclaw/workspace/state.json  34.7 KB  1.0 KB  ...  CRUZÓ EL UMBRAL (+33.7 KB)

  [MEDIO] E-08 — 1 archivo de arranque por encima de su umbral
        Evidencia:   workspace/state.json: 34.7 KB (umbral 1.0 KB), sin cambio desde 2026-07-31. Cada
                     agente lee estos archivos completos al arrancar cada sesión, [...] El costo en
                     dólares no es derivable: haría falta saber cuántas sesiones abre cada agente y
                     con qué modelo.
```

---

## Cómo avisa

Cuando un archivo cruza su umbral, sale como hallazgo **`E-08`** por el **mismo
mecanismo que la herramienta ya usa** para todo lo demás — no se inventó ningún canal
nuevo:

- La sección `HALLAZGOS Y TIPS` del comando `advise`.
- El JSON de `/api/advice` (campo `bootFiles` + el hallazgo en `findings`).
- El dashboard, que consume ese mismo JSON.

Detalles del hallazgo:

- **Impacto:** medio. No escala a "acción" por recurrencia, y eso es correcto por
  construcción: `splitEscalated` reproduce `findings()` sobre ventanas pasadas de
  registros, y `E-08` no nace de registros de uso, así que nunca aparece en esa
  historia reproducida. Se queda como tip.
- **`Metric`:** share de archivos por encima de su umbral **sobre los archivos que
  se pudieron medir**. Un archivo ilegible no está ni por encima ni por debajo;
  contarlo en cualquiera de los dos lados haría que el número dijera algo que el
  dato no dice.
- **`SavingsUSD`: 0, y la evidencia lo dice explícitamente.** El costo en dólares
  de un archivo de arranque **no es derivable** desde esta máquina: depende de
  cuántas sesiones se abran, con qué modelo, y con qué precio de entrada — nada de
  eso está en el dato que se midió. Se reporta el peso y el delta, no un ahorro
  inventado.

### Archivos que no se pueden medir

Un archivo faltante, sin permiso de lectura, o que resulta ser un directorio se
reporta como **`no derivable: <motivo>`**. Nunca como `0 bytes`, que se leería como
una medición y como "no creció".

---

## El snapshot: la única excepción con estado

La herramienta no recuerda nada entre corridas — todo se deriva de los registros de uso.
El tamaño de un archivo es la excepción: **el disco no guarda su propia historia**,
así que sin un snapshot no hay delta posible.

- **Dónde:** `~/.local/state/llm-agent-spend-manager/bootfiles.json` (mismo
  directorio de estado que `cap.db`, ver `internal/enforce`).
- **Qué guarda:** por archivo, el último tamaño **distinto** observado y cuándo se
  observó. Por eso el delta significa **"cambio desde que el tamaño fue distinto"**
  y no "desde la corrida anterior": bajo un dashboard que mide cada minuto, lo
  segundo reportaría `0` para siempre.
- **Quién escribe:** **solo** el comando `advise` de la CLI. El dashboard mide pero
  no avanza el snapshot — corre bajo `ProtectHome=read-only`
  (`docs/servicios-permanentes.md`) y además no debe reiniciar el delta con cada
  poll del navegador.
- **Si falta:** no es un error, es "primera corrida" (sin delta). Si está corrupto,
  **sí** es un error: un snapshot ilegible se dice, no se ignora.

---

## Configuración

| Variable | Default | Formato |
|---|---|---|
| `SPEND_BOOT_FILES` | los dos archivos de la tabla de arriba | `ruta=bytes` separados por comas |

Ejemplo:

```
SPEND_BOOT_FILES="/home/x/.openclaw/workspace/state.json=67584,/home/x/.openclaw/workspace/SYNC.md=131072"
```

Una variable **puesta pero inválida es un error**, no un fallback silencioso a los
defaults: un typo en una ruta dejaría al checker vigilando calladamente un archivo
que nadie pidió.

---

## Cuándo recalibrar

Recalibra (y actualiza este documento con la fecha y la medición nueva) cuando:

1. Se limpie `SYNC.md` con un criterio explícito → su baseline pasa a ser un
   known-good y debe reemplazarse aquí.
2. `E-08` se levante y la revisión humana concluya que el tamaño **está
   justificado** → el baseline creció legítimamente; súbelo con la evidencia.
3. Haya varias corridas de historia acumuladas en el snapshot → el `2×` de un solo
   ancla se puede reemplazar por algo derivado de crecimiento observado.

Añadir rutas nuevas a la vigilancia pide un **hallazgo real** que las justifique,
igual que estas dos. Una lista genérica de "archivos que suenan importantes" no es
una medición.
