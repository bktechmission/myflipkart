# MyFlipKart architecture

The reference for every MyFlipKart tool. Each system has one page with a diagram
(editable Excalidraw source plus a rendered image), the request flow, the guards and
the failure modes.

| System | What it covers | Page |
|---|---|---|
| **Ask Sauby chat** | orchestrator, RAG search, LLM gateway, order tools and guardrails, human handoff | [chat.md](chat.md) |
| **ReviewGuard** | product reviews: rules, authenticity, LLM tagger, verifier, decisions, moderation, appeals | [reviewguard.md](reviewguard.md) |
| **Data stores** | WAL transactions, conversations, telemetry, indexes, files | [data-stores.md](data-stores.md) |
| **Auth** | OIDC sign-in (Google, MockID), sessions, roles, CSRF, ownership | [auth.md](auth.md) |
| **Observability and evals** | metrics, logs, dashboards, eval suites, release gates, canary | [observability-evals.md](observability-evals.md) |

Animated walkthroughs (GitHub Pages): [a chat turn](https://bktechmission.github.io/myflipkart/) ·
[one review through ReviewGuard](https://bktechmission.github.io/myflipkart/reviewguard.html).

## Diagrams

| Diagram | Editable source | Rendered |
|---|---|---|
| Chat (whole system) | [../architecture.excalidraw](../architecture.excalidraw) | [../architecture.png](../architecture.png) |
| ReviewGuard | [reviewguard.excalidraw](reviewguard.excalidraw) | [reviewguard.svg](reviewguard.svg) |
| Data stores | [data-stores.excalidraw](data-stores.excalidraw) | [data-stores.svg](data-stores.svg) |
| Auth | [auth.excalidraw](auth.excalidraw) | [auth.svg](auth.svg) |
| Observability and evals | [observability-evals.excalidraw](observability-evals.excalidraw) | [observability-evals.svg](observability-evals.svg) |

The `.excalidraw` files open at [excalidraw.com](https://excalidraw.com). The ReviewGuard, data, auth
and observability diagrams are generated from one spec by `python3 docs/tools/make_diagrams.py`
(source and SVG together, so they never drift); the chat diagram by `docs/tools/make_architecture.py`
(its PNG is exported from Excalidraw).

## Keep it current

Every feature change updates its architecture page and diagram **in the same PR**.

- New behaviour, guard or failure mode: add it to the system's page.
- New component or data flow: update the diagram spec and regenerate (`make_diagrams.py`).
- New system: add a page here and a row to the tables above.

`make test` enforces part of this: `backend/internal/api/architecture_test.go` fails when a
top-level package in `backend/internal/` or `backend/cmd/`, or an HTTP route, isn't mentioned on any
page in this folder.
