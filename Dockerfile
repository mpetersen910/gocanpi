FROM --platform=$BUILDPLATFORM golang:1.26.6 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /gocanpi .

FROM scratch
COPY --from=build /gocanpi /gocanpi
ENTRYPOINT ["/gocanpi"]