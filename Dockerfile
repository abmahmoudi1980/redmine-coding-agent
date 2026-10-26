FROM node:24-bookworm-slim AS web

WORKDIR /src/web

COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY web/ ./
RUN npm run build

FROM golang:1.24-bookworm AS build

WORKDIR /src

COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=web /src/internal/web/dist ./internal/web/dist

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/redmine-agent ./cmd/redmine-agent

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/redmine-agent /usr/local/bin/redmine-agent

USER nonroot:nonroot

EXPOSE 8080

VOLUME ["/data", "/etc/redmine-agent"]

ENTRYPOINT ["/usr/local/bin/redmine-agent"]
CMD ["serve", "--config", "/etc/redmine-agent/config.yaml"]
