# Ask Sauby (MyFlipKart.com): notes for Claude Code

A public, MIT-licensed learning project: a Go backend (standard library only) and a TypeScript UI.
`main` auto-deploys the public demo (https://asksauby.onrender.com): work on a branch and get the
merge approved.

## Rules

- `make test` (race detector on) and `make gate` must pass before every push. Never weaken a test or gate.
- Public repo hygiene: no secrets, no personal data, synthetic data only and labelled synthetic, no real
  brand or person names for sellers, offensive placeholder terms mild and clearly made up.
- Heavy runs on the shared machine: `flock /tmp/claude-0/qk-stack.lock <cmd>`.
- UI changes: check 390 px and desktop, light and dark.

## Keep the architecture current

Every feature change updates its page and diagram in `docs/architecture/` **in the same PR**:
- new behaviour, guard or failure mode → the system's page;
- new component or data flow → the diagram spec in `docs/tools/make_diagrams.py` (regenerate the
  `.excalidraw` + `.svg`), or `docs/tools/make_architecture.py` for the chat diagram;
- new system → a new page plus a row in `docs/architecture/README.md`.

`backend/internal/api/architecture_test.go` fails when a top-level package in `backend/internal/` or
`backend/cmd/`, or an HTTP route, has no architecture page mentioning it.

## Walkthroughs (GitHub Pages, served from `docs/`)

`docs/src/*.ts` compile into `docs/assets/*.js` with `cd web && npx tsc -p ../docs`; commit the output.
