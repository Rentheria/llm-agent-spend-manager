# llm-agent-spend-manager — wrapper de escritorio (Tauri)

App de escritorio (Windows / macOS / Linux) sobre el **mismo** dashboard web del binario Go.
No reimplementa nada: al arrancar lanza `llm-agent-spend-manager serve --local --port 4600` y muestra
`http://127.0.0.1:4600` en una ventana nativa (WebView del sistema). Así el desktop nunca se
desincroniza del CLI ni de la PWA — el binario Go sigue siendo la fuente de verdad de UI y datos.

> **Estado:** scaffold Tauri v2 **sin compilar aquí** (esta máquina no tiene toolchain Rust).
> Estructura y configs listas; falta `cargo`/`tauri-cli` para construir y probar los instaladores.

## Requisitos para construir
- Rust (stable ≥ 1.77) + `cargo`.
- `tauri-cli` v2: `cargo install tauri-cli --version '^2'`.
- Dependencias de sistema de Tauri (WebView2 en Windows, WebKitGTK en Linux, Xcode CLT en macOS)
  — ver https://tauri.app/start/prerequisites/.
- El binario `llm-agent-spend-manager` compilado (`go build ./cmd/llm-agent-spend-manager`), **junto al
  ejecutable de la app** o en el `PATH` (el wrapper lo busca primero al lado del `.exe`/binario,
  luego en `PATH` — ver `src-tauri/src/lib.rs`).

## Construir / correr
```bash
# desde desktop/src-tauri
cargo tauri dev      # desarrollo (abre la ventana, lanza el server)
cargo tauri build    # instaladores por plataforma en target/release/bundle/
```

## Iconos
`src-tauri/icons/icon.png` es la fuente (el medidor "$" del tema). Para regenerar el set completo
por plataforma (`.ico`, `.icns`, PNGs):
```bash
cargo tauri icon src-tauri/icons/icon.png
```

## Notas de diseño
- La ventana apunta a una URL externa local (`http://127.0.0.1:4600`), no a `frontendDist`.
  `desktop/frontend/index.html` es solo un placeholder ("Iniciando…") que se ve si el server
  aún no levantó.
- El server hijo se lanza en `setup()` y se mata al cerrar la ventana (`WindowEvent::Destroyed`).
- `--local`: el desktop usa el server solo en loopback; el acceso LAN/QR es cosa del `serve` del
  CLI, no del wrapper.
- Alternativa futura: empaquetar el binario Go como **sidecar** de Tauri (`externalBin`) para que
  el instalador lo incluya y no dependa del `PATH`.
