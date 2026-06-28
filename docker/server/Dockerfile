# revquic-broker (server) — Alpine, multi-stage. Build context = the revquic module root.
FROM golang:1.25-alpine AS build
ENV GOPROXY=direct GOFLAGS=-mod=mod CGO_ENABLED=0
RUN apk add --no-cache git
WORKDIR /src
COPY . .
RUN go build -o /out/revquic-broker ./cmd/revquic-broker

FROM alpine:3.20
COPY --from=build /out/revquic-broker /usr/local/bin/revquic-broker
EXPOSE 4242/udp 8080/tcp
ENTRYPOINT ["revquic-broker"]
