# Calibración real de los factores de estimación

> **Qué es esto:** los adaptadores de **actividad estimada** (Cursor, Antigravity)
> convierten señales de actividad en un **rango de tokens**, nunca un punto, nunca
> gasto real (ver `docs/architecture.md` §3.3). Esos factores nacieron
> provisionales, marcados 🏠, puestos por heurística razonable pero **sin calibrar
> con datos reales**. Este documento fija/documenta el **número base inicial con
> evidencia** medida contra los datos que YA existen en esta máquina — el paso que
> la propuesta aprobada (§4, "Camino B") pedía y que no se había hecho.
>
> **Regla que se respeta aquí:** no se inventa ningún número. Si un factor no se
> puede mejorar con los datos ya existentes (sin generar tráfico artificial ni
> reverse-engineering de formatos binarios fuera de alcance), se deja como estaba y
> se dice explícitamente, con la razón. Eso es información real, no un hueco.
>
> Fecha de la corrida: 2026-07-26. Toda la evidencia se midió en vivo contra los
> archivos reales de esta máquina con el mismo driver read-only pure-Go que usan
> los adaptadores (`internal/sqliteutil`, `modernc.org/sqlite`), no de memoria.

---

## Método (Camino B, calibración de base)

El factor `tokens_por_unidad_de_actividad` de Camino B se calibra comparando, en las
mismas conversaciones, dos señales que sí existen en disco:

1. **La actividad visible** que el adaptador cuenta como unidad (para Cursor:
   `ai_code_hashes`; para Antigravity: `steps`).
2. **Un piso de tokens reales** derivado del crudo por conversación (Camino A) —
   cuando ese crudo es texto recuperable.

Donde ambas señales coexisten, el cociente `Σ tokens_piso / Σ actividad` da el
factor **medido** de esta máquina. Es **distinto** del auto-calibrado que ya corre
en caliente en runtime (`deriveTokensPerCodeHash`): aquí el objetivo es **fijar el
número base inicial documentado con evidencia**, no solo dejarlo auto-calibrando
cada vez (y que sirve además de fallback cuando una conversación no trae crudo).

---

## Cursor — tokenizer real (A2, 2026-07-30) y `tokens_por_code_hash` RE-calibrado

**Fuentes** (`internal/adapters/cursor/`):
- `~/.cursor/ai-tracking/ai-code-tracking.db`, tabla `ai_code_hashes` → actividad
  (code hashes) + modelo por conversación (Camino B).
- `~/.cursor/chats/<ws>/<uuid>/store.db`, tabla `blobs` → el texto de la
  conversación; el piso (Camino A) es ese texto **tokenizado de verdad**.

### El hallazgo: el error grande no era el ratio, era el denominador

`bytes/4` sonaba a "aproximación de ±20%". La medición dice otra cosa. Censo de
los 19 `store.db` de esta máquina (58.8 MB de blobs, 2026-07-30):

| Qué hay en `blobs` | Bytes | % |
|---|---|---|
| Mensajes JSON (`{"role":…,"content":…}`) — texto que SÍ se le mandó al modelo | 4.9 MB de texto extraíble | **8.4%** |
| Estado del editor en protobuf (URIs del workspace, snapshots de archivos, checkpoints, IDs opacos) | 53.9 MB | **91.6%** |

El 91.6% se estaba contando como si fuera prompt. Y encima el 81% de esos bytes
son imprimibles, así que "se ven" como texto — por eso nadie lo cachó leyendo el
código. Consecuencias medidas:

| | Antes (`Σ LENGTH(data) / 4`) | Ahora (texto real + tokenizer) |
|---|---|---|
| Tokens-piso de Cursor (todo el historial) | 14,705,429 | **1,351,308** (÷10.9) |
| Rango de Cursor en `status --window all` | el ancho de arriba, ×10.9 | **÷10.9, mismo método** |
| Ancho del rango de la FLOTA completa | **±13%** | **±1.1%** |

(De esa última fila hay que leer el **ancho**, no el punto: los totales absolutos suben entre
corridas simplemente porque los agentes siguen trabajando.)

Y es la que más importa: **la incertidumbre de toda la flota estaba dominada por la
estimación de Cursor**, no por los agentes medidos.

El ratio bytes/token del texto que sí es texto resultó **3.64** — o sea el `4`
nunca estuvo tan mal *como ratio*. Estaba mal *lo que se dividía entre 4*.

### Qué cuenta y qué no (regla de la casa: contar lo demostrable, declarar el resto)

Cuenta: `system`/`user` (string), texto del `assistant`, nombre + argumentos de
`tool-call`, y el `result` del `tool-result` (venga en texto o en JSON). No
cuenta: los blobs protobuf, los archivos cacheados sin `role` (su contenido ya
viene dentro del tool-result que los trajo), y `providerOptions` — que guarda una
**segunda copia** del mismo turno (`anthropicNativeContent`) y habría duplicado
cada mensaje del assistant.

Los blobs protobuf **no se declaran cero**: se declaran **indecidibles desde
disco**. Nada en disco dice cuáles de esos snapshots se mandaron como contexto.
Lo no medible se cobra donde siempre: en el techo del rango (`invisibleHeadroom`),
a la vista.

### `defaultTokensPerCodeHash`: 1,745 → **101**

El factor de Camino B se mide contra el piso de Camino A, así que al cambiar el
piso **había que re-derivarlo o quedaría 11× inflado**.

| Métrica | Antes (2026-07-26) | Ahora (2026-07-30) |
|---|---|---|
| Conversaciones con **ambas** señales | 2 | **14** |
| Σ tokens-piso | 4,484,296 | 1,174,539 |
| Σ code hashes | 2,570 | 11,588 |
| **Factor POOLED (Σvis/Σhash)** | 1,745 | **101** |
| Rango por conversación | 1,139 – 3,989 | **42.6 – 3,768** |

### Dónde SÍ aplica este 101 (y dónde no aplica nunca)

`deriveTokensPerCodeHash` **gana siempre** que exista al menos una conversación con ambas
señales: recalcula el factor en caliente contra el disco de esa máquina. Verificado: compilar con
1745 y con 101 da **exactamente la misma salida** en esta máquina — la constante ni se toca.

O sea el 101 es el **default de arranque**: aplica en una instalación fresca, o en una máquina
donde ningún `store.db` sea legible. Ahí es donde el 1745 viejo habría metido un error de ~17×, y
por eso re-derivarlo no era opcional aunque aquí no se note.

### Caveats honestos de esta calibración

- **La dispersión por conversación es de ~90×** (42.6 vs 3,768 tokens/hash),
  contra ~3.5× de la medición vieja. La muestra creció de 2 a 14 conversaciones y
  lo que se ve con más datos es que **el factor está débilmente determinado**: un
  code-hash no es una unidad estable de trabajo. El pooled queda dominado por las
  conversaciones con más hashes. Por eso el producto sigue mostrando **rango, no
  punto**, y el auto-calibrado en caliente (`deriveTokensPerCodeHash`) sigue vivo
  para no fosilizar el número.
- **El tokenizer es de otra familia.** `o200k_base` es de OpenAI; Anthropic no
  publica el suyo. Para una conversación con un modelo Claude, el conteo **no es
  el de Anthropic**. Lo que sí cambió es la *clase* de error: antes se asumía un
  ratio único para prosa, código, JSON y base64; ahora cada uno se segmenta como
  lo que es, que es de donde venía la mayor parte del error en un corpus casi
  todo código y salida de herramientas.
- **No se pudo validar contra ground truth de Anthropic, y se intentó.** Los
  transcripts de Claude Code emparejan `output_tokens` con un texto que es solo
  **parte** de lo facturado: el pensamiento se guarda resumido. Medido sobre 2,155
  mensajes con bloque de thinking, el texto guardado tokeniza a **~25%** del
  conteo facturado; en 1,068 mensajes sin thinking, a ~57%. Comparar contra eso
  mediría los huecos del transcript, no el error del tokenizer. Por eso el número
  **no sube de tier**: sigue siendo `≈ actividad estimada`.
- **Costo:** el `status` completo pasó de 4.6 s a 6.2 s (+1.6 s) por leer y
  tokenizar los blobs en vez de solo pedir su `LENGTH`. Se paga una vez por
  corrida y compra un orden de magnitud de exactitud.

### Factor que sigue SIN poder mejorarse con datos existentes

- **`invisibleHeadroom = 2.0`** — sube el techo del rango para cubrir el costo
  invisible (re-lecturas de contexto, caché) que el texto guardado cuenta una sola
  vez. Para calibrarlo haría falta el **costo/tokens total real** de la
  conversación y compararlo contra el piso visible. Cursor **no registra ni costo
  ni tokens totales**, así que no hay señal local contra la cual medirlo. Se queda
  en 2.0 (techo ancho y honesto). **Mejora real posible:** solo con sondas
  facturadas (tráfico controlado que sí reporte tokens), que no se corren sin
  permiso explícito.

---

## Antigravity — la banda tokens/unidad NO se puede calibrar con datos locales; la **unidad** sí se corrigió (A3)

> ⚠️ **SUPERADO EN PARTE.** Todo lo que sigue en esta sección describe el estado
> hasta A3, cuando Antigravity era **solo Camino B**. Después se abrió Camino A: el
> protobuf se mapeó por formato de cable y **el piso de tokens ya está medido**
> (467,307 tokens sobre las 12 conversaciones), lo que **reemplazó** la banda
> 2300/17250 de A3. Lo que sigue se conserva porque documenta cómo se llegó ahí y qué
> se descartó en el camino. **La sección vigente es
> [§Antigravity — Camino A](#antigravity--camino-a)**, al final.

**Fuente** (`internal/adapters/antigravity/`): `~/.gemini/antigravity-cli/
conversations/<uuid>.db`, tablas `gen_metadata` (una fila = una generación real del
modelo) y `steps` (una fila = un step). **Solo Camino B** (el crudo es protobuf sin
documentar → nada de A, por diseño de la propuesta §5).

Dos cosas distintas, no confundirlas:

- **El factor tokens/unidad sigue SIN calibrar** — no hay verdad-de-terreno de
  tokens en disco (evidencia abajo, 2026-07-26). Sin cambio.
- **La unidad sí se cambió** de `step` a `generación` (**A3, 2026-07-31**), porque
  eso los datos locales sí lo miden. Detalle al final de esta sección.

### Qué se buscó (para no dejar la calibración sin intentar)

Se inspeccionaron **las 6 conversaciones** en disco (66 steps en total), su esquema
completo y **todos** los blobs de todas las tablas (`gen_metadata`,
`executor_metadata`, `trajectory_metadata_blob`, `steps`), extrayendo cada cadena
ASCII legible dentro del protobuf.

### Hallazgo central: NO hay conteo de tokens en ninguna parte

- No existe columna ni campo de tokens/usage en el esquema (tablas: `trajectory_meta`,
  `steps`, `gen_metadata`, `executor_metadata`, `parent_references`,
  `trajectory_metadata_blob`, `battle_mode_infos`).
- Las 48 coincidencias de la palabra "token" en los blobs resultaron ser **prosa no
  relacionada** de system prompts / skills ("*token-efficient version of
  transcript*", "*whitespace separated tokens (words)*", "*design system with all
  tokens*" = design tokens de CSS). **Cero** son conteos de uso.
- **Conclusión:** no hay verdad-de-terreno de tokens en los datos locales de
  Antigravity. Por tanto la banda `tokensPerStepLow/High` **no se puede fijar a un
  número medido con los datos que ya existen** — se **deja como estaba (800–6000)**.
  Mejorarla exigiría una de dos vías, ambas fuera del alcance aprobado:
  1. **Reverse-engineering del protobuf + tokenizer real** (Camino A) — explícitamente
     fuera de alcance para Antigravity hasta que el formato esté documentado; frágil.
  2. **Tráfico artificial facturado** contra Antigravity para medir tokens reales por
     step — no se hace sin permiso explícito.

### Señales cruzadas reales SÍ encontradas (leads, no aplicadas aún)

Aunque no dan tokens, la exploración descubrió dos señales reales que valen la pena
registrar para una futura mejora (ninguna se implementó: hacerlo sería Camino A,
fuera de alcance):

1. **El modelo realmente usado SÍ es recuperable** por conversación desde el
   protobuf de `gen_metadata` (flag `used_claude` + nombre limpio), un modelo único
   y claro por conversación en la muestra:

   | conversación | steps | generaciones | modelo usado |
   |---|---|---|---|
   | `0433a414-…` | 38 | 10 | `claude-opus-4-6-thinking` |
   | `08cac20b-…` | 8  | 2  | `gemini-pro-default` |
   | `21ff6b85-…` | 4  | 1  | `gemini-3.5-flash-low` |
   | `27437b46-…` | 8  | 2  | `gemini-pro-default` |
   | `3def5812-…` | 4  | 1  | `gemini-3.5-flash-low` |
   | `70b91544-…` | 4  | 1  | `gemini-3.5-flash-low` |

   Esto **matiza** la suposición actual del adaptador ("*los blobs traen el menú de
   modelos, no el usado*"): al menos en esta muestra, el modelo usado es legible y
   único. Actuar sobre esto (parsear el protobuf para etiquetar modelo por conv)
   sería Camino A → fuera de alcance por ahora, pero es el lead más fuerte si algún
   día se abre A para Antigravity.

2. **`gen_metadata` cuenta generaciones reales del modelo**: es una unidad de
   actividad más fiel que el `step` crudo (un step de UI/plan no pesa como una
   generación completa). ✅ **Lead aplicado en A3 (2026-07-31)** — ver abajo. Sigue
   sin resolver el factor tokens/unidad, que necesita la verdad-de-terreno de tokens
   que aquí no existe.

   > ⚠️ **Corrección a la redacción original de este lead.** Decía "*cada fila lleva
   > un `req_vrtx_…`*" y "*66 steps → 17 generaciones (~3.9 steps por generación)*".
   > Ambas cifras/afirmaciones quedaron **superadas y son inexactas**:
   > - El `req_vrtx_…` (ID de request de Vertex AI) **solo aparece en las
   >   conversaciones respaldadas por Claude**. Las de Gemini tienen filas de
   >   `gen_metadata` sin ningún `req_vrtx_`. Contar IDs habría subcontado las
   >   conversaciones Gemini → **se cuenta la fila, no el ID** (ver A3).
   > - El ~3.9 salía de una muestra de 6 conversaciones/66 steps. Medido de nuevo
   >   sobre **las 12 conversaciones en disco** (2026-07-31): **2.875**, no 3.9.

### Decisión (2026-07-26)

- **Banda `tokensPerStepLow/High` = 800–6000: SIN CAMBIO.** No hay evidencia local
  para mejorarla y no se inventa precisión inexistente. Los dos leads (modelo
  recuperable, generaciones como unidad) quedan documentados para una futura
  apertura de Camino A o una sonda facturada con permiso.

---

### A3 (2026-07-31) — la unidad pasa de `step` a `generación`

#### Paso 0: se confirmó que esto es **Camino B**, no A

El DDL real de la tabla es:

```sql
CREATE TABLE gen_metadata (idx integer, data blob, size integer NOT NULL DEFAULT 0, PRIMARY KEY (idx))
```

Contar generaciones es `SELECT COUNT(*) FROM gen_metadata`: **estructuralmente
idéntico** al `SELECT COUNT(*) FROM steps` que el adaptador ya hacía. **El blob
`data` nunca se abre** en el código entregado — no se decodifica protobuf, no se
lee `used_claude`, no se lee el modelo. Por eso sigue siendo Camino B.

(Durante el diagnóstico sí se escanearon blobs en una sonda desechable, para
verificar que `COUNT(*)` no sobrecuenta; esa sonda se borró y **nada de eso vive en
el código**. Ver "Qué se verificó antes de confiar en `COUNT(*)`".)

#### El pool medido (mismo método Σ/Σ que `tokens_por_code_hash` de Cursor)

La banda no se inventó de nuevo: es la banda de step **re-expresada por generación**,
escalada por la razón steps/generación **medida en esta máquina**, sumando todas las
conversaciones disponibles (no promediando una muestra):

| conversación | steps | generaciones | steps/gen |
|---|---|---|---|
| `0433a414…` | 38 | 10 | 3.80 |
| `08cac20b…` | 8 | 2 | 4.00 |
| `17557a60…` | 40 | 16 | 2.50 |
| `21ff6b85…` | 4 | 1 | 4.00 |
| `27437b46…` | 8 | 2 | 4.00 |
| `3def5812…` | 4 | 1 | 4.00 |
| `70a22fde…` | 23 | 8 | 2.88 |
| `70b91544…` | 4 | 1 | 4.00 |
| `a898c84b…` | 47 | 23 | 2.04 |
| `d6ba4ba7…` | 13 | 6 | 2.17 |
| `f3ab0004…` | 81 | 31 | 2.61 |
| `f7c32b32…` | 52 | 11 | 4.73 |
| **POOL** | **322** | **112** | **2.8750** |

```
tokensPerGenerationLow  = 800  × 322/112 = 2300    (división exacta)
tokensPerGenerationHigh = 6000 × 322/112 = 17250   (división exacta)
```

**🏠 PROVISIONAL — y hay que decirlo, no esconderlo:** 12 conversaciones es una
muestra muy chica, y la razón por conversación **se dispersa de verdad** (2.04 …
4.73, más del doble de extremo a extremo). El 2.875 es un número **medido**, no
inventado, pero **no es un número estable**. Con más conversaciones se moverá.

#### Fallback documentado

Si una conversación **no tiene tabla `gen_metadata`** (formato viejo o ajeno),
`generations = 0` y el estimado **cae a la banda por steps de hoy** (`steps ×
800–6000`) — **no a `0`** (borraría actividad real) **ni a un número inventado**. La
condición exacta es "la tabla no existe / el `COUNT` falla", no "la conversación
tiene cero generaciones".

⚠️ **Este fallback no se pudo ejercitar contra datos reales**: las 12 conversaciones
en disco **todas** tienen `gen_metadata`. Está cubierto solo por test con fixture
(`TestCollectUsage_FallsBackToStepsWithoutGenMetadata`). Se dice explícito en vez de
insinuar que fue verificado en vivo.

#### Qué se verificó antes de confiar en `COUNT(*)`

- **Sospecha:** la última fila de cada DB es enorme (103 KB–397 KB) vs. el resto
  (~1 KB) — ¿sería un snapshot/agregado, y entonces `COUNT(*)` sobrecontaría en 1
  por conversación? **No.** El blob grande **no contiene** los request-IDs de las
  filas chicas (0 de 9, 0 de 15, 0 de 31, 0 de 11) y trae su propio ID distinto,
  cronológicamente el último → **es una generación real con payload grande**.
  `COUNT(*)` es correcto.
- **Caveat conocido:** 2 filas de las 112 traían **dos** request-IDs cada una
  (`f3ab0004` idx=3, `f7c32b32` idx=8). O sea el conteo por fila **subcuenta
  marginalmente** los requests (probablemente reintentos). Se acepta: contar IDs
  volvería la unidad dependiente del modelo (los Gemini no traen ID).
- **Caveat conocido:** `17557a60` tiene un WAL de 1.6 MB y el modo `mode=ro` puede
  no incluirlo. Es **preexistente** (ya afectaba a `steps`), y como ambos conteos
  salen de **un solo `Open`**, la razón queda internamente consistente.

#### Efecto real contra los DBs de esta máquina (no teórico)

El total de la flota **no se mueve** — es una consecuencia aritmética de escalar por
el pool (Σsteps×800 ≡ Σgens×2300 por construcción), no una casualidad. Lo que cambia
es **el reparto entre conversaciones**, que es justo el punto: quien tiene muchos
steps por generación deja de pesar de más.

| conversación | steps | gens | ANTES (steps×800–6000) | DESPUÉS (gens×2300–17250) |
|---|---|---|---|---|
| `d6ba4ba7…` | 13 | 6 | 10,400–78,000 | 13,800–103,500 ↑ |
| `f7c32b32…` | 52 | 11 | 41,600–312,000 | 25,300–189,750 ↓ |
| `17557a60…` | 40 | 16 | 32,000–240,000 | 36,800–276,000 ↑ |
| `f3ab0004…` | 81 | 31 | 64,800–486,000 | 71,300–534,750 ↑ |
| `a898c84b…` | 47 | 23 | 37,600–282,000 | 52,900–396,750 ↑ |
| `70a22fde…` | 23 | 8 | 18,400–138,000 | 18,400–138,000 (=) |
| `70b91544…` | 4 | 1 | 3,200–24,000 | 2,300–17,250 ↓ |
| `0433a414…` | 38 | 10 | 30,400–228,000 | 23,000–172,500 ↓ |
| `08cac20b…` | 8 | 2 | 6,400–48,000 | 4,600–34,500 ↓ |
| `27437b46…` | 8 | 2 | 6,400–48,000 | 4,600–34,500 ↓ |
| `3def5812…` | 4 | 1 | 3,200–24,000 | 2,300–17,250 ↓ |
| `21ff6b85…` | 4 | 1 | 3,200–24,000 | 2,300–17,250 ↓ |
| **TOTAL** | 322 | 112 | **257,600–1,932,000** | **257,600–1,932,000** |

5 conversaciones suben, 6 bajan, 1 queda igual (`70a22fde`, 23/8 = 2.875 exacto = la
razón del pool).

#### Lo que A3 **no** hizo

- No decodifica protobuf ni lee `used_claude`/modelo por conversación (Camino A,
  fuera de alcance).
- No tocó `invisibleHeadroom` ni nada de Cursor.
- No corrió tráfico artificial facturado contra Antigravity (requiere permiso
  explícito del dueño de la cuenta).
- **No mejoró la banda tokens/unidad** — sigue 🏠 sin calibrar, por las mismas
  razones de 2026-07-26. Solo cambió la unidad a la que la banda se engancha.

---

## Antigravity — Camino A

**Veredicto: VIABLE, con alcance acotado.** El crudo de Antigravity SÍ se pudo leer
sin `.proto` publicado, y el texto que contiene SÍ se pudo tokenizar de verdad. Eso
convirtió la banda inventada de A3 en un **piso medido**. Lo que NO se volvió medible
es el techo: sigue siendo `piso × (1 + headroom)`, declarado, no calculado.

Las 12 conversaciones de esta máquina decodificaron **12/12**, y **12/12** entregaron
modelo. Cero conversaciones cayeron al fallback.

### Cómo se leyó un protobuf sin `.proto`

Google no publica el esquema de `gen_metadata.data`. Se recuperó por **formato de
cable** (`internal/adapters/antigravity/blob.go`): el wire format de protobuf es
auto-delimitado — cada campo trae `(número << 3) | tipo`, y con el tipo basta para
saltar el valor sin conocer su nombre. Caminando esa estructura se obtiene el árbol
de campos; para cada campo `length-delimited` se decide **submensaje vs. string**
intentando parsearlo como submensaje y, si falla, exigiendo que ≥90 % de sus runes
sean texto imprimible (`printableTextRatio`). Sin heurística de nombres, sin adivinar
semántica: si un campo no se puede decidir, se descarta.

El mapa de campos resultante está en `blob.go` documentado **como observación, no
como contrato** — es lo que estos 12 DBs muestran hoy, y puede cambiar sin aviso.
Toda función del decodificador falla en suave a `""`.

### Qué se cuenta y qué no (misma regla de la casa que Cursor/A2)

Se cuenta **una sola vez** el texto que el store demuestra que fue al modelo: system
prompt, texto de cada mensaje, thinking, nombres+argumentos de tool calls, y las
definiciones de herramientas (nombre, descripción, schema). Se tokeniza con
`internal/tokenize` (BPE real `o200k_base`), el mismo camino que usa Cursor.

Se **excluyó a propósito**, para no inflar el piso por doble conteo — verificado
contra los DBs reales, no asumido:

| Campo | Por qué no se cuenta | Verificación |
|---|---|---|
| request f16 (system prompt troceado) | es el mismo texto de f1, partido en chunks | **131/131** chunks son substring de f1 |
| tool-def f7 | duplica el nombre de la herramienta (f1) | **189/189** iguales |
| tool-call f9 | duplica el nombre de la llamada (f2) | **154/154** iguales |

Y se declara **indecidible** (no se cuenta como 0, se paga en el headroom): las
firmas opacas de pensamiento de Vertex (mensaje f20, tool-call f7) y los protos de
resultado estructurado (mensaje f23/f25). No son texto legible, así que contarlos
sería inventar un número.

Un detalle que sí cambió el resultado: por conversación se toma **la fila más
grande, no la suma**. Solo una fila carga el historial final completo; sumarlas
contaría la conversación entera tantas veces como generaciones tuvo. Verificado:
**12/12** conversaciones tienen exactamente 1 fila con payload.

### El pool medido (Σ/Σ, mismo método que `tokens_por_code_hash`)

| conversación | steps | gens | piso medido | tok/gen | modelo |
|---|---|---|---|---|---|
| `d6ba4ba7…` | 13 | 6 | 53,769 | 8,961 | gemini-3.6-flash-tiered |
| `f7c32b32…` | 52 | 11 | 59,773 | 5,433 | claude-opus-4-6-thinking |
| `17557a60…` | 40 | 16 | 56,261 | 3,516 | claude-opus-4-6-thinking |
| `f3ab0004…` | 81 | 31 | 88,758 | 2,863 | claude-opus-4-6-thinking |
| `a898c84b…` | 47 | 23 | 53,612 | 2,330 | gemini-3.6-flash-tiered |
| `70a22fde…` | 23 | 8 | 33,510 | 4,188 | gemini-3.6-flash-tiered |
| `70b91544…` | 4 | 1 | 16,199 | 16,199 | gemini-default |
| `0433a414…` | 38 | 10 | 37,825 | 3,782 | claude-opus-4-6-thinking |
| `08cac20b…` | 8 | 2 | 17,749 | 8,874 | gemini-pro-default |
| `27437b46…` | 8 | 2 | 17,422 | 8,711 | gemini-pro-default |
| `3def5812…` | 4 | 1 | 16,215 | 16,215 | gemini-3-flash-a |
| `21ff6b85…` | 4 | 1 | 16,214 | 16,214 | gemini-3-flash-a |
| **TOTAL** | **322** | **112** | **467,307** | **4,172** | — |

- `defaultTokensPerGeneration` = 467,307 / 112 = **4,172**
- `tokensPerStepFloor` = 467,307 / 322 = **1,451**

Pooled, no promediado por conversación: una conversación de 31 generaciones debe
pesar 31× una de 1. `deriveTokensPerGeneration()` recalcula esto en vivo sobre la
máquina donde corre; la constante es solo el fallback para cuando nada decodifica.

### Lo que esto le hizo a la banda de A3

La banda `2300 / 17250` de A3 no era conservadora, era **incorrecta por abajo**: su
extremo BAJO quedaba **por debajo del piso realmente medido** (2,300 < 4,172), o sea
que lo que se reportaba como "mínimo" era menos que el texto que el disco demuestra
que existió. Ese es exactamente el error que Camino A venía a corregir.

Los `800 / 6000` por step que A3 heredaba también se reemplazaron por el 1,451 medido.

### Modelo → `$` (segundo entregable)

El modelo por conversación sí es recuperable (`used_claude` + nombre, campo 19 del
request). Se resuelve por **votación entre filas** (`dominantModel`) en vez de confiar
en una sola: 111/112 filas lo traían, y la votación tolera la que no.

Con eso enchufado a `internal/pricing` (`PerTokenBounds`, extremo bajo a precio de
input y alto a precio de output), Antigravity **ya reporta `$`** donde antes había `—`:

```
Antigravity   ≈$1.21–$18.20   ≈467,307–1,401,921   12 (+8 sin precio)
```

El `$1.21` bajo es exactamente la suma de los pisos medidos valuados a precio de
input. Las **8 conversaciones Gemini siguen sin precio a propósito**: sus ids no
están en la tabla de `pricing`, y un modelo desconocido debe rendir "sin precio", no
un `$0` silenciosamente falso. `claude-opus-4-6-thinking` sí resuelve, porque
`pricing` ahora recorta el sufijo `-thinking` (mismo modelo, mismas tarifas: el
thinking se factura como output normal).

**Caveat que no se puede omitir en ninguna superficie:** si Antigravity corre **gratis**
bajo una suscripción, ese `$` es peso relativo / costo de oportunidad contra precio de
lista público, **no un cargo real**.

### Caveats honestos

- 🏠 **Muestra de 12 conversaciones.** Es poco, y el valor por conversación **sí se
  dispersa fuerte**: 2,330 … 16,215 tok/gen, según cuánta salida de herramientas
  arrastró cada una. Es un número **medido**, no uno **estable**.
- **Es un piso, no un consumo.** Cuenta el historial almacenado UNA vez; cada
  generación reenvió el system prompt, las tool definitions y todo el prefijo otra
  vez. Ese re-envío no está en el piso — vive en el headroom.
- **`invisibleHeadroom` sigue en 2.0 y sigue sin calibrar**, por las mismas razones
  que en Cursor: no hay verdad-de-terreno local contra la cual medirlo. Además la
  tabla `steps` guarda ~1.0 MB más de texto (snapshots de archivos del editor, turnos
  de subagentes, errores de herramientas) que el disco **no** demuestra que haya
  entrado al contexto.
- **Tokenizer cross-family.** `o200k_base` no es el de Anthropic; para las
  conversaciones Claude el conteo es aproximación de otra familia (ver §Cursor).
- **El formato no está documentado y puede romperse sin aviso.** Por eso Camino B se
  conservó como degradación explícita, en este orden: piso medido → generaciones ×
  factor → steps × piso por step. Nunca un 0, nunca un número inventado.
- Sigue en pie de 2026-07-26: **no existe conteo de tokens en ningún lado del
  esquema**. Camino A no lo encontró — lo reemplazó midiendo el texto.

### Lo que Camino A **no** hizo

- No corrió tráfico facturado contra Antigravity (sigue requiriendo permiso explícito).
- No mejoró `invisibleHeadroom` ni tocó nada de Cursor.
- No convirtió a Antigravity en tier medido: sigue etiquetado **"actividad
  estimada"** con `≈`. Lo que subió de calidad es el piso del rango, no la
  confianza del total.
