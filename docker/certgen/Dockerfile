# revquic-certgen — generates the mTLS cert bundle into /certs (a shared volume). Context = revquic root.
FROM golang:1.25-alpine AS build
ENV GOPROXY=direct GOFLAGS=-mod=mod CGO_ENABLED=0
RUN apk add --no-cache git
WORKDIR /src
COPY . .
RUN go build -o /out/revquic-certgen ./cmd/revquic-certgen

FROM alpine:3.20
COPY --from=build /out/revquic-certgen /usr/local/bin/revquic-certgen
ENTRYPOINT ["revquic-certgen"]
