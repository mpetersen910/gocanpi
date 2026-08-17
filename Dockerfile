FROM --platform=$BUILDPLATFORM golang:1.26.6 AS build
ARG TARGETARCH
ARG TARGET=./cmd/colcan
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -ldflags="-s -w" -o /out/app ${TARGET}

FROM scratch
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]