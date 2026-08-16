# vela-worker runtime.
#
# It clones freshly-forked Vela foundation apps and runs their test suite
# locally, so it needs the same toolchain the foundation's Dockerfile `test`
# stage assembles: Ruby 4.0.5, build tooling, libpq/libvips/libyaml, git,
# Node (for the foundation's JS/importmap steps), and a local PostgreSQL the
# suite can talk to over the socket.
#
# This image runs in its OWN container on the Hetzner box with:
#   - NO docker.sock            (it cannot touch the deck or other containers)
#   - NO Holodex build secret   (it verifies via job tickets, cannot deploy)
#   - egress ALLOWED            (git, rubygems, the model API)
# The app containers it produces are the exact opposite (locked down, no egress).

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/vela-worker ./worker

FROM ruby:4.0.5-slim
ENV DEBIAN_FRONTEND=noninteractive \
    BUNDLE_PATH=/usr/local/bundle \
    LANG=C.UTF-8 \
    WORKER_DATA=/work

# Runtime + build deps mirroring the foundation Dockerfile, plus postgres (the
# local test DB), git, and node.
RUN apt-get update -qq && \
    apt-get install --no-install-recommends -y \
      build-essential git curl ca-certificates \
      libpq-dev libvips libyaml-dev pkg-config \
      postgresql postgresql-client \
      nodejs npm ripgrep && \
    rm -rf /var/lib/apt/lists/*

# The worker daemon starts postgres per job via `service postgresql start`
# inside the sandbox shell (same command the foundation Dockerfile uses), so
# the cluster just needs to exist. A superuser role matching the runtime user
# is created so peer auth over the socket works.
RUN service postgresql start && \
    su postgres -c "createuser --superuser root" || true

COPY --from=build /out/vela-worker /usr/local/bin/vela-worker
RUN mkdir -p /work

EXPOSE 8790
# Runs as root so it can start postgres and manage /work; the container itself
# is the isolation boundary (no docker.sock, no deck secrets).
CMD ["vela-worker"]
