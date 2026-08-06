# Stack tecnológico y decisiones (con el porqué)

Este doc explica el CÓMO — lenguaje, frameworks, herramientas — y por qué se descartó cada alternativa considerada. El QUÉ/diseño vive en `architecture.md`. Todas las decisiones de esta página se revisaron y confirmaron el 2026-07-26, priorizando: transparencia, seguridad y facilidad de uso/configuración **para cualquier usuario de la comunidad**, no solo para quien lo escribió.

## Núcleo (CLI + adaptadores + enforcement opcional): Go

Decisión final, revisada dos veces el mismo día (se consideró Node/TypeScript primero, luego se corrigió a Go).

**Por qué Go:**
1. **Instalación real más fácil para cualquiera**: compila a un solo binario estático por sistema operativo — nada de "primero instala Node.js/npm". Un escalón menos de fricción para la comunidad.
2. **Menos superficie de ataque real**: la librería estándar de Go cubre servidor HTTP y (vía `embed`) empotrar los archivos del dashboard dentro del mismo binario — cero dependencias npm/terceros que auditar. Los ataques de cadena de suministro en npm son un riesgo activo y documentado; esto lo evita de raíz.
3. **Transparencia**: un binario compilado desde un repo público es más fácil de verificar/reproducir que una instalación con decenas de paquetes de terceros.
4. **Cross-compile nativo**: Go compila para Linux/Mac/Windows desde una sola máquina sin herramientas extra.

**Costo real, asumido con los ojos abiertos:**
- Go no es el lenguaje del resto del stack de quien lo escribió (TypeScript en todos lados) — más lento de construir al inicio por la curva de aprendizaje.
- `llm-budget-cap` y `chatarmor` **NO cambian** — siguen en TypeScript, son librerías npm por naturaleza (requisito distinto al de este CLI). Este proyecto reusa el **algoritmo** de `llm-budget-cap` (el script Lua `ATOMIC_BUDGET_CAP_LUA`, que es Lua puro para Redis y agnóstico de lenguaje), ejecutado vía un cliente Redis de Go (ej. `go-redis`, maduro y mantenido) — no se importa el paquete npm directo.

**Alternativas consideradas y descartadas:**
- **Node.js/TypeScript** (elección inicial): mismo lenguaje que el resto del stack, pero requiere que el usuario final tenga Node instalado, y el árbol de dependencias npm es una superficie de ataque real que Go evita.
- **Rust**: mismos beneficios de "nativo"/memoria segura que Go, pero curva de aprendizaje mucho más alta para el equipo — costo de velocidad no justificado para un proyecto de comunidad/side. Tauri (ver abajo) sí usa Rust, pero eso es una dependencia empaquetada y probada, no código propio que el equipo tenga que escribir/mantener en Rust.

## Terminal

El propio binario Go (ej. `llm-agent-spend-manager status`, tabla en texto). Viene gratis con el núcleo, cero trabajo extra.

## Desktop (Windows/Mac/Linux): Tauri

Wrapper Tauri (Rust por debajo, más liviano que Electron) sobre el dashboard web servido por el núcleo Go — una sola base de UI para los 3 sistemas operativos, comunicándose con el núcleo por la API local.

**Por qué NO Flutter** (se propuso, se consideró en serio, y se descartó con razones concretas — no fue un "no" reflejo):
1. Ya hay un solo dashboard web planeado — meter Flutter encima serían 2 UIs distintas para los mismos datos, duplicación real.
2. Distribución de Flutter desktop tiene fricción real: macOS pide notarización (Apple Developer, ~$99/año) o Gatekeeper avienta advertencias a cualquiera que lo baje — mala primera experiencia para adopción OSS.
3. Un binario Flutter para una tabla de gasto es sobre-ingeniería, choca con "chiquito y enfocado".
4. Las herramientas de referencia ya validadas (`claude-usage`, `Claude-Code-Usage-Monitor`) son CLI/terminal, no apps de escritorio — la comunidad target ya vive ahí.

Flutter queda como posible fase futura **solo si** el MVP demuestra uso real y de verdad falta push nativo — no como apuesta de entrada.

## Mobile: PWA + acceso de red local (sin Tailscale obligatorio)

- El mismo dashboard web como PWA instalable desde el navegador del celular. Cero paso por App Store/Play Store.
- **Acceso por defecto, sin instalar nada:** dashboard accesible en la IP LAN de la máquina (`192.168.x.x:puerto`) — si el celular está en el mismo WiFi, ya funciona.
- **Código QR** al arrancar el CLI para no ni siquiera teclear la IP.
- **Acceso remoto**: opcional/avanzado, sin exigir herramienta específica (Tailscale/ngrok/túnel propio, lo que el usuario ya tenga). Se descartó Tailscale como parte del camino por defecto porque exigir su instalación+config es fricción real para cualquier usuario.
- **Firebase** es una ruta de crecimiento **opt-in** válida para acceso remoto fácil + push real — pero implica que el dato sale de la máquina local hacia la nube de Google, así que nunca es el default.

## Enforcement (fase 2, opcional): SQLite embebido; Redis opt-in

**Nada externo por default.** El tope combinado vive en un archivo SQLite que la herramienta crea sola, con el driver que ya está linkeado para los adapters (modernc.org/sqlite, Go puro, sin CGO). Persiste al reiniciar y su candado de escritura coordina **entre procesos** en la misma máquina — que era lo único por lo que se había considerado Redis.

Redis queda **detrás de la interfaz `enforce.Counter`**, activable con `--redis`, para el caso que sí lo exige: varias instancias del proxy compartiendo un tope. Ahí el cliente `go-redis` (maduro, mantenido) ejecuta el script Lua ya validado de `llm-budget-cap`, sin reinventar el algoritmo ni importar el paquete npm — que es exactamente el problema para el que ese algoritmo se escribió. Ver `decisions.md` (2026-07-30).

## Resumen de la matriz de decisión

| Pieza | Elección | Alternativas descartadas |
|---|---|---|
| Núcleo/CLI | Go | Node/TypeScript, Rust |
| Desktop | Tauri | Electron, Flutter |
| Mobile | PWA + red local/QR | Flutter, app nativa por SO |
| Acceso remoto | Opcional (Tailscale/ngrok/propio) o Firebase opt-in | Tailscale como requisito default |
| Enforcement | Sliding window sobre SQLite embebido por default; Redis + go-redis opt-in para topes entre máquinas | LiteLLM Proxy, **ventana fija** (deja pasar 2× en la frontera), **exigir Redis para instalar**, escribir un daemon propio tipo Redis |
