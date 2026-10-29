FROM rust:1.98.0-alpine AS agent-build
RUN --mount=type=secret,id=laptop_ca,target=/etc/ssl/certs/ca-certificates.crt \
    apk add --no-cache musl-dev
WORKDIR /agent
COPY agent-rust/ ./
RUN --mount=type=secret,id=laptop_ca,target=/etc/ssl/certs/ca-certificates.crt \
    cargo +1.98.0 build --locked --release
RUN mkdir /licenses && \
    for directory in /usr/local/cargo/registry/src/*/*; do \
      [ -d "$directory" ] || continue; \
      destination="/licenses/$(basename "$directory")"; \
      mkdir -p "$destination"; \
      find "$directory" -maxdepth 1 -type f \( -iname 'license*' -o -iname 'notice*' -o -iname 'copying*' -o -iname 'copyright*' \) \
        -exec cp '{}' "$destination/" \;; \
    done

FROM scratch AS agent-export
COPY --from=agent-build /agent/target/release/monitoring-agent /monitoring-agent

FROM --platform=$BUILDPLATFORM golang:1.27.0-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=laptop_ca,target=/etc/ssl/certs/ca-certificates.crt \
    go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /monitoring ./cmd/monitoring

# Preserve the notices required when redistributing the compiled dependencies.
RUN CGO_ENABLED=0 go list -deps -f '{{with .Module}}{{if not .Main}}{{.Dir}}{{end}}{{end}}' ./cmd/monitoring > /tmp/module-dirs \
    && sort -u -o /tmp/module-dirs /tmp/module-dirs \
    && cache="$(go env GOMODCACHE)" \
    && mkdir /licenses \
    && while IFS= read -r directory; do \
         [ -n "$directory" ] || continue; \
         destination="/licenses/${directory#"$cache"/}"; \
         mkdir -p "$destination"; \
         find "$directory" -maxdepth 1 -type f \( -iname 'license*' -o -iname 'notice*' -o -iname 'copying*' -o -iname 'copyright*' \) \
           -exec cp '{}' "$destination/" \;; \
       done < /tmp/module-dirs \
    && cp /usr/local/go/LICENSE /licenses/GO-LICENSE

FROM build AS integration
COPY --from=agent-build /agent/target/release/monitoring-agent /monitoring-agent
ENV MONITORING_AGENT_BINARY=/monitoring-agent
RUN CGO_ENABLED=0 go test -c -tags integration -o /integration.test ./tests/integration
ENTRYPOINT ["/integration.test", "-test.v"]

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /monitoring /monitoring
COPY --from=agent-build /agent/target/release/monitoring-agent /monitoring-agent
COPY --from=agent-build /licenses /licenses/rust/
COPY --from=build /licenses /licenses
COPY --from=build /src/internal/hub/static/echarts.*.txt /licenses/echarts/
USER 65532:65532
ENTRYPOINT ["/monitoring"]
