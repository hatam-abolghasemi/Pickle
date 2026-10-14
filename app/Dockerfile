FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /pickle .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pickle /pickle
USER 65532:65532
ENTRYPOINT ["/pickle"]
