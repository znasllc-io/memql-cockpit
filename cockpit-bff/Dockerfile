# syntax=docker/dockerfile:1
#
# memQL Cockpit BFF (cockpit-bff) carrier-binary Dockerfile.
#
# Per-client BFF (znasllc-io/memql-cockpit#288): the Cockpit's own
# product-neutral backend-for-frontend -- the engine `bff` role with NO
# product DSL, owned by this repo. The Cockpit connects to it at
# cockpit.<domain>.
#
# The build context spans BOTH memql/ AND memql-cockpit/cockpit-bff/ as
# siblings under the context root, because cockpit-bff's go.mod carries
#   replace github.com/znasllc-io/memql => ../../memql
# pointing at the local engine sibling. Docker resolves that relative
# path the same way local `go build` does. The build-cockpit-bff-image
# workflow assembles the context by checking out this repo at
# workspace/memql-cockpit and the memql sibling at workspace/memql, then
# building with:
#     context: workspace
#     dockerfile: workspace/memql-cockpit/cockpit-bff/Dockerfile
#
# Unlike a PRODUCT carrier (memql-bff-copresent), this needs no go.work,
# no product DSL tree, no templ generate / tailwind build: the engine
# commits its generated *_templ.go, and the `bff` role links no product
# code. The result is byte-for-byte the engine's own bff surface.

# ---------------------------------------------------------------------------
# Stage 1: compile the cockpit-bff binary (engine bff role, CGO-free).
# ---------------------------------------------------------------------------
FROM golang:1.26.4-alpine AS build

RUN apk add --no-cache git bash
WORKDIR /build

# Full source for both modules. cockpit-bff/go.mod's `replace ../../memql`
# resolves to /build/memql once both are in place.
COPY memql/ memql/
COPY memql-cockpit/cockpit-bff/ memql-cockpit/cockpit-bff/

# Build the cockpit-bff binary. BUILD_TAGS=bff is the engine default
# (backend-for-frontend); left as an ARG so the same Dockerfile can
# produce other node-type variants later if the Cockpit ever needs them.
ARG BUILD_TAGS=bff
WORKDIR /build/memql-cockpit/cockpit-bff
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -tags "${BUILD_TAGS}" -ldflags="-s -w" -o cockpit-bff .

# Reuse the engine's healthcheck binary -- same /healthz probe contract.
WORKDIR /build/memql
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/memql-cockpit/cockpit-bff/healthcheck ./cmd/healthcheck

# ---------------------------------------------------------------------------
# Stage 2: runtime.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/base-debian12

WORKDIR /app

COPY --from=build /build/memql-cockpit/cockpit-bff/cockpit-bff ./memql
COPY --from=build /build/memql-cockpit/cockpit-bff/healthcheck ./healthcheck

# The engine DSL baseline is embedded in the binary via go:embed; the
# on-disk copy is kept only so a runtime MEMQL_DSL_PATH override works.
COPY --from=build /build/memql/dsl ./dsl
COPY --from=build /build/memql/VERSION ./VERSION

EXPOSE 8085 50051

ENTRYPOINT ["/app/memql"]
