FROM golang:1.21 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -mod=mod -a -o freezer-daemon ./cmd/daemon

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/freezer-daemon .
USER 0:0

ENTRYPOINT ["/freezer-daemon"]
