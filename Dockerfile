# Ask Sauby · MyFlipKart.com — one image: Go server + compiled TypeScript UI + embedded demo sign-in.
# Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
#
# Cloudflare Containers run linux/amd64. Build locally with:
#   docker build --platform linux/amd64 -t asksauby .
#   docker run -p 8080:8080 asksauby          # http://localhost:8080

# 1) TypeScript UI
FROM --platform=linux/amd64 node:20-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npx tsc -p .

# 2) Go server (standard library only, static binary)
FROM --platform=linux/amd64 golang:1.22-alpine AS server
WORKDIR /src/backend
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# 3) Minimal runtime: no shell, runs as an unprivileged user
FROM --platform=linux/amd64 gcr.io/distroless/static-debian12:nonroot
COPY --from=server /out/server /app/server
COPY --from=web /src/web/public /app/web
COPY backend/evals/gates.json /app/evals/gates.json
# Public demo defaults. Secrets are generated in memory at start (nothing baked in).
# Disk is ephemeral on Cloudflare: the demo resets whenever the container sleeps.
ENV ADDR=:8080 \
    PUBLIC_URL=auto \
    PUBLIC_DEMO=1 \
    MOCK_EMBEDDED=1 \
    DATA_DIR=/tmp/sauby \
    WEB_DIR=/app/web \
    EVAL_GATES=/app/evals/gates.json \
    EMBED_BACKEND=hash
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/server"]
