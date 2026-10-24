# syntax=docker/dockerfile:1

# Every stage runs on the build machine's own architecture and Go cross-compiles,
# so building for arm or mips needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS src
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod assets.go ./
COPY agent ./agent
COPY shortcuts ./shortcuts
COPY hub ./hub
COPY agentbin/.gitkeep ./agentbin/.gitkeep

# The Linux agents the hub hands out to new machines (embedded gzipped).
FROM src AS agents
RUN for a in amd64 arm64; do \
      GOOS=linux GOARCH=$a go build -ldflags="-s -w" -o agentbin/linux-$a ./agent \
      && gzip -9nf agentbin/linux-$a; \
    done

# docker build --target test .
FROM agents AS test
RUN test -z "$(gofmt -l .)" && go vet ./... && go test ./...

FROM agents AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
RUN if [ "$TARGETARCH" = arm ]; then export GOARM=${TARGETVARIANT#v}; fi; \
    GOOS=$TARGETOS GOARCH=$TARGETARCH GOMIPS=softfloat \
      go build -ldflags="-s -w -X main.version=$VERSION" -o /out/hub ./hub \
    && mkdir -p /rootfs/data

# docker build --target bin --output type=local,dest=out .   (just the binary)
FROM scratch AS bin
COPY --from=build /out/hub /hub

# The image: one static binary, no OS underneath.
FROM scratch
COPY --from=build --chown=10001:10001 /rootfs/data /data
COPY --from=build /out/hub /hub
USER 10001:10001
ENV CLIP_DATA=/data GOGC=50
VOLUME /data
EXPOSE 8766
HEALTHCHECK --interval=30s --timeout=3s CMD ["/hub", "healthcheck"]
ENTRYPOINT ["/hub"]
