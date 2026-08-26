# webui-src

TypeScript + React source for the account-management admin UI served at `/ui`.
Built with Vite; output is committed to `internal/app/webui/dist/` and embedded
into the Go binary via `//go:embed webui/dist` in `internal/app/ui.go`.

```sh
cd internal/app/webui-src
npm install
npm run build
```

`npm run build` regenerates `internal/app/webui/dist/`. There is no CI in this
repo yet and `go build` needs the embedded files to exist at Go-build time, so
the build output is committed alongside the TS source — anyone editing the UI
must rebuild and commit `internal/app/webui/dist/` in the same change.
