# syntax=docker/dockerfile:1

# The builder's Go must be at least the version go.mod declares. There is no
# way to read that from here, so the two are pinned together by a CI check.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, in their own layer: they change far less often than the
# code, so an edit to a .go file does not re-download the module graph.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# CGO_ENABLED=0 for a static binary the distroless static image can run.
# -trimpath so the build is reproducible: without it the binary embeds the
# absolute build path, and two builds of the same commit differ.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/taskapi ./cmd/taskapi

# Distroless static: no shell, no package manager, no libc, non-root by
# default. Roughly 15 MB, and nothing in it to exploit if the process is.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/taskapi /taskapi

USER nonroot:nonroot
EXPOSE 8080 9090

# There is no curl and no shell to run one, which is why the binary probes
# itself. The subcommand reads the same configuration as the process it is
# probing, so a non-default --admin.addr is honoured.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/taskapi", "healthcheck"]

ENTRYPOINT ["/taskapi"]

# Both halves in one process. compose overrides this to run serve and worker
# separately, which is what shows the queue is genuinely decoupled.
CMD ["all"]
