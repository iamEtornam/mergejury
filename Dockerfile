# Container image for the landing page (cmd/site).
#
# Build context is the repository root, not site/: the server embeds the page
# at compile time, so the build needs go.mod and the site/ directory together.
#
# Dokploy / Coolify / Railway: Build Type "Dockerfile", Build Path "/",
# exposed port 8080.
#
#   docker build -t mergejury-site .
#   docker run --rm -p 8080:8080 mergejury-site

FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first so edits to the page do not re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/site ./cmd/site
COPY site ./site
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/mergejury-site ./cmd/site

# Everything is embedded in the binary, so the runtime image needs nothing
# else: no shell, no package manager, no writable filesystem.
FROM scratch
COPY --from=build /out/mergejury-site /mergejury-site

# PORT makes the server bind :$PORT, i.e. all interfaces — 127.0.0.1 would be
# unreachable from outside the container. Set as an env var rather than a CMD
# flag so a platform that injects its own PORT (Dokploy, Railway, Fly) wins
# without overriding the entrypoint.
ENV PORT=8080
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/mergejury-site"]
