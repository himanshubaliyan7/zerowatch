# syntax=docker/dockerfile:1.7
# ZeroWatch server image: one static binary for every role
# (all | ingest | query | storage | supervisor), with the web UI embedded
# (web/dist is go:embed'ed at compile time, so `npm run build` first if the UI
# changed) and the compiled eBPF object next to it.
#
# Build from the repository root:
#   docker build -f deploy/docker/Dockerfile -t zerowatch:dev .
#
# Build args:
#   EBPF=true|false  build with -tags ebpf (default true). The ebpf loader falls
#                    back to the /proc probe at runtime when the kernel, tracefs
#                    or the capabilities don't allow it, so true is safe.
#   VERSION          stamped into the image label.

ARG GO_IMAGE=golang:1.23
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build
ARG EBPF=true
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY ebpf ./ebpf
COPY web/embed.go ./web/embed.go
COPY web/dist ./web/dist
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    tags=""; if [ "$EBPF" = "true" ]; then tags="ebpf"; fi; \
    CGO_ENABLED=0 GOOS=linux go build -tags "$tags" -trimpath -ldflags="-s -w" -o /out/zerowatch ./cmd/zerowatch && \
    mkdir -p /out/data && cp ebpf/red_probe.bpf.o /out/red_probe.bpf.o

FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
LABEL org.opencontainers.image.title="zerowatch" \
      org.opencontainers.image.description="ZeroWatch: self-configuring observability (metrics, logs, traces, alarms, UI)" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/zerowatch /usr/bin/zerowatch
COPY --from=build /out/red_probe.bpf.o /usr/share/zerowatch/red_probe.bpf.o
# distroless:nonroot is uid/gid 65532; /data is the default data dir.
COPY --from=build --chown=65532:65532 /out/data /data
ENV ZW_DATA_DIR=/data \
    ZW_BPF_OBJECT=/usr/share/zerowatch/red_probe.bpf.o
USER 65532:65532
WORKDIR /data
# UI/API + CloudWatch, OTLP/HTTP, Prometheus remote write, StatsD (UDP), gossip.
EXPOSE 8080 4318 8428 8125/udp 7946 7946/udp
ENTRYPOINT ["/usr/bin/zerowatch"]
CMD ["--role=all"]
