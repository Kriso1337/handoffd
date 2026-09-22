FROM golang:1.25-alpine AS source
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

FROM source AS test
RUN go vet ./... && go test ./...

FROM source AS build
ARG GOOS=darwin
ARG GOARCH=arm64
RUN CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH \
    go build -trimpath -ldflags="-s -w" -o /out/handoffd ./cmd/handoffd

FROM scratch AS artifact
COPY --from=build /out/handoffd /handoffd
