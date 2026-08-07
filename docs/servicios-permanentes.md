# Correrlo permanente (systemd user)

> El proxy **topa en tokens reales** del `usage` que reporta el proveedor
> (`--cap … --window 5h --unit tokens`). La unidad importa: topar por bytes estimados de la
> petición se intentó y bajo prompt caching el estimado se fue ~7×. Ver «Operación del proxy»
> abajo.

Un medidor que hay que acordarse de prender no mide: mide *los ratos en que alguien se acordó*.
Los procesos de esta herramienta corren como **servicios de usuario** — sin `sudo`, sin
root, sin unidades de sistema:

| Servicio | Qué hace | Escucha | Se cae → |
|---|---|---|---|
| `lasm-proxy` | Cuenta los **tokens reales** de cada llamada de la flota **y la topa**: `--cap 120000000 --window 5h --unit tokens` | `127.0.0.1:4610` | **Los agentes desatendidos se quedan sin LLM.** Ver abajo. |
| `lasm-proxy-interactive` | El mismo proxy contra el **mismo** contador, pero `--cap 0`: cuenta la sesión interactiva y **nunca** la bloquea, para que un tope pensado para trabajo de fondo no corte a la persona que está esperando | `127.0.0.1:4611` | **La sesión interactiva se queda sin LLM** (connection refused); el `.timer` de guardia quita el override. |
| `lasm-dashboard` | Sirve el dashboard y la API de lectura | `127.0.0.1:4600` | Solo se pierde la vista; nadie deja de trabajar. |

Las unidades viven en **`deploy/systemd/`** y usan `%h`/`%S`, así que no traen el `$HOME` de nadie
adentro.

## Instalar

```bash
go build -o ~/.local/bin/llm-agent-spend-manager ./cmd/llm-agent-spend-manager
install -Dm644 deploy/systemd/lasm-dashboard.service ~/.config/systemd/user/lasm-dashboard.service
install -Dm644 deploy/systemd/lasm-proxy.service     ~/.config/systemd/user/lasm-proxy.service
systemctl --user daemon-reload
systemctl --user enable --now lasm-dashboard.service   # dashboard: sin riesgo, préndelo
# El proxy NO se prende sin leer docs/enforcement-cableado.md primero: cablearlo
# mete el proxy en la ruta crítica de tus agentes.
```

**`enable` no basta para que arranque en boot.** Un servicio de usuario solo corre cuando hay
sesión del usuario; sin sesión, systemd los apaga al hacer logout y no los levanta al bootear:

```bash
sudo loginctl enable-linger $USER   # verificar: loginctl show-user $USER -p Linger  → Linger=yes
```

Sin `Linger=yes`, "permanente" no quiere decir permanente.

## Por qué cada flag está donde está

**Dashboard**
- **Sin `--lan`.** Loopback es el default y así se queda: `--lan` publica el gasto de toda la flota
  en la red local. Es un flag para prenderlo a mano un rato, no para dejarlo en una unidad.
- **`--cache-ttl 60s`** (default 10s). Un escaneo completo cuesta **~6 s de CPU y ~79 MB de pico**
  (medido, historial completo). Nadie mira esto cada 10 s, y navegando entre vistas un TTL corto
  encima rescans. **Idle no escanea**: el scan es por request, así que un servicio prendido y sin
  visitas cuesta ~4 MB y 0% de CPU (medido).
- **`ProtectHome=read-only`.** El dashboard **lee** logs de agentes y no escribe nada; que el kernel
  lo haga cumplir cuesta una línea.

**Proxy** — su cableado está en **`docs/enforcement-cableado.md`** (ir ahí antes de tocarlo). El
número del tope no se elige al tanteo: ponlo por DEBAJO del techo calibrado de tu ventana, que
`quota` reporta con su rango y su dispersión. Un tope por encima del techo real no topa nada, y
uno demasiado abajo corta trabajo legítimo.

**Los dos**
- **`Restart=always` + `RestartSec=2`.** En el proxy no es decoración: si muere, los agentes que
  apuntan a `127.0.0.1:4610` se quedan sin salida. Verificado con `kill -9` en ambos.
- **`MemoryMax=256M` / `CPUQuota=50%`.** ~3× sobre el pico medido, y siguen siendo techo real: si
  la máquina la comparten varios agentes, sin estos límites un escaneo pesado se lleva por delante
  a los demás (OOM kills).
- **`ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`.**

## Verificación en vivo — dashboard

Qué comprobar después de instalarlo, y con qué (los resultados de la última columna son los de una
corrida de referencia, no una promesa):

| # | Qué se probó | Cómo | Resultado |
|---|---|---|---|
| A | Responde en loopback | `curl 127.0.0.1:4600/` | **HTTP 200**, 0.8 ms |
| B | La API trae datos reales, no una cáscara | `curl /api/summary` | los agentes detectados, con `grand.totalTokens` distinto de cero |
| C | No escucha fuera de loopback | `ss -ltn \| grep 4600` | `127.0.0.1:4600` — **no** `0.0.0.0` |
| D | Sobrevive un `kill -9` | matar el MainPID | `NRestarts=1`, `active`, **HTTP 200** a los 4 s |
| E | Arranca en boot | `is-enabled` + `Linger` | `enabled` + `Linger=yes` |
| F | No pesa idle | `MemoryCurrent` | **4.4 MB** contra un techo de 256 MB |

Un reinicio del servicio **no reinicia el conteo**: el contador sigue en su misma llave
(`tokens:fleet:default`) porque vive en SQLite y no en memoria. Vale la pena verificarlo tras el
primer `restart`.

## Operación del proxy — prender, apagar, mirar

```bash
# estado + el banner, que dice el tope vigente en una línea
systemctl --user status lasm-proxy.service
journalctl --user -u lasm-proxy.service -n 30 --no-pager
#   → "Modo: cuenta y TOPA en 120000000 tokens (~120.0M) por 5h0m0s rodante (tokens)"
#   (si dice "context-bytes", el banner además avisa que esa unidad es un ESTIMADO)

# lo que suma cada llamada, con los cuatro buckets del `usage` a la vista
journalctl --user -u lasm-proxy.service --no-pager | grep 'cap: +'
#   → "cap: +91099 tokens (in=2 out=4 cache_write=7867 cache_read=83226)
#       current=165247/120000000 | key=tokens:fleet:default path=\"/v1/messages\""

# seguir el log en vivo (aquí aparecen los 429 cuando el tope corta)
journalctl --user -u lasm-proxy.service -f

# reiniciar tras cambiar la unidad
install -Dm644 deploy/systemd/lasm-proxy.service ~/.config/systemd/user/lasm-proxy.service
systemctl --user daemon-reload && systemctl --user restart lasm-proxy.service

# cuánto se lleva gastado de la ventana en curso
curl -sD- -o/dev/null 127.0.0.1:4610/v1/messages | grep -i '^x-cap'
#   X-Cap-Limit / X-Cap-Remaining / X-Cap-Enforced
```

**Cómo se ve cuando topa** (probado en corrida controlada, puerto 4611 y `--state memory`, sin tocar
producción): **HTTP 429** con `Retry-After: 60`, `X-Cap-Remaining: 0` y el cuerpo
`combined budget cap reached (60000/50000)`. El upstream **no** se toca.

Cuando el upstream es Anthropic, ese cuerpo trae además cuándo refila la ventana **real** de 5 h —
`… — la ventana real de 5 h de Anthropic se libera en 1 h 34 min (~21:12)` (T139, ADR-13). Es otro
reloj que el del tope: el contador corre sobre una ventana anclada a la época, así que "topaste" y
"Anthropic te cortó" no son lo mismo y el mensaje ahora los distingue. Si dice que no hay ventana en
vuelo, el 429 es solo del tope propio y subirlo desbloquea la flota; si da una hora, hay que
esperarla. La fase se lee en segundo plano (un escaneo completo cuesta ~11.5 s), así que el primer
429 tras arrancar el servicio puede llegar sin ETA y decirlo con esas palabras.

Dos cosas que hay que saber de antemano, porque en la emergencia no es momento de descubrirlas:

- **Los clientes no dicen "topaste el presupuesto".** Un CLI headless honra el `Retry-After` y se
  queda reintentando hasta su propio timeout (verificado: rc=124). Es backpressure correcta, pero es un
  fallo **menos legible** que el mensaje de cuota de Anthropic. Si un agente se queda "colgado", el
  primer lugar a mirar es el `journalctl` de arriba.
- **Cambiar `--window` descarta el conteo.** El índice de bucket se calcula sobre la duración de la
  ventana, así que al cambiarla la fila vieja se borra y la ventana arranca limpia. No es bug; es la
  razón por la que no se cambia el valor a la ligera.
- **Cambiar `--unit` también arranca ventana limpia**, y a propósito: la llave lleva la unidad
  adelante (`tokens:fleet:default`). Sin eso, pasar de bytes a tokens leería los BYTES acumulados
  como si fueran tokens y daría 429 a toda la flota al primer request.

**Si hay que levantar el tope de emergencia** (el tope frena a la flota, no al proveedor — subirlo no
crea cuota, solo mueve el muro de lugar): editar `--cap` en la unidad, `daemon-reload`, `restart`.
Dejar `--window` como está para no perder el acumulado.

## Apagar

```bash
systemctl --user disable --now lasm-dashboard.service
systemctl --user disable --now lasm-proxy.service
```

Apagar el proxy **no** es suficiente para desconectar a los agentes: hay que quitarle también el
`ANTHROPIC_BASE_URL` de `~/.claude/settings.json`, o se quedan apuntando a un puerto muerto. El
procedimiento completo de reversa está en `docs/enforcement-cableado.md`.
