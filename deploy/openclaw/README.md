# `deploy/openclaw/` — scripts que se ENTREGAN, no se instalan solos

Estos scripts corren desde `~/.local/bin/`, no desde aquí. Esta carpeta es la copia **versionada**:
un original suelto en un dotfile sin repo se lo lleva un `rm` distraído o un disco perdido. La copia
de aquí es la que se revisa en un diff y la que sobrevive.

| Script | Qué hace | Doc |
|---|---|---|
| `advise-notify.sh` | Notificador de la automejora; imprime `NO_REPLY` cuando no hay nada nuevo | `docs/automejora.md` |

## Instalar

```bash
install -m755 deploy/openclaw/advise-notify.sh ~/.local/bin/advise-notify.sh

# chequeo de deriva: la copia viva y la versionada deben ser idénticas
diff -q ~/.local/bin/advise-notify.sh deploy/openclaw/advise-notify.sh || echo "DERIVÓ"
```

Las copias se mantienen **byte a byte iguales** a propósito — así ese `diff -q` es una prueba de
deriva válida. Si a un script hay que agregarle una nota "esta es la copia del repo", esa nota rompe
justo la comprobación que la haría útil; por eso las notas van aquí y no adentro.

## Engancharlo a un cron de OpenClaw

`advise-notify.sh` no se agenda solo. La línea que lo engancha:

```bash
openclaw cron create \
  --name "spend-advise" \
  --schedule "0 9 * * *" \
  --command "$HOME/.local/bin/advise-notify.sh"
```

Un job `--command` entrega su stdout al chat del agente. El script imprime `NO_REPLY` cuando no hay
hallazgos nuevos, y OpenClaw usa ese token para **suprimir** la entrega: una mañana tranquila no
manda mensaje y no gasta un turno de modelo. Ese es el motivo de que la deduplicación viva en el
script y no en el binario — el medidor no recuerda lo que ya dijo, el notificador sí.
