> 🌐 **Español** (este archivo) · [**English**](#branch-convention-english)

# Contribuir

## Convención de ramas

Este repo usa dos ramas de larga vida:

- **`main`** — estable / producción. **Nunca se commitea directo a `main`.**
- **`dev`** — donde se hace todo el trabajo activo.

Flujo:

1. Todos los cambios van a **`dev`** (o a una rama de feature que sale de `dev` y se
   mergea de vuelta a `dev`).
2. Cuando `dev` está listo y estable, se mergea **`dev` → `main`**.
3. `main` siempre debe poder buildearse y pasar los checks (`go build ./...`,
   `go vet ./...`, `go test -p 1 ./...`).

`main` es la rama default del repo en GitHub.

---

<a id="branch-convention-english"></a>
> 🌐 [**Español**](#contribuir) · **English** (this section)

# Contributing

## Branch convention

This repo uses two long-lived branches:

- **`main`** — stable / production. **Never commit directly to `main`.**
- **`dev`** — where all active work happens.

Flow:

1. All changes go to **`dev`** (or a feature branch off `dev` that merges back into `dev`).
2. When `dev` is ready and stable, merge **`dev` → `main`**.
3. `main` must always build and pass checks (`go build ./...`, `go vet ./...`,
   `go test -p 1 ./...`).

`main` is the repository's default branch on GitHub.
