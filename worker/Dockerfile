# vela-worker: the deploy relay. It receives a repo+sha and two signed
# tickets from Vela, has Holodex verify the commit, and ships it. No model,
# no shell, no toolchain — every production failure the fat version ever had
# came from the brain it no longer contains.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/vela-worker ./worker

FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/vela-worker /usr/local/bin/vela-worker
EXPOSE 8790
CMD ["vela-worker"]
