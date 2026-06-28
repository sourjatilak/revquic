# revquic-exit — Alpine + iproute2/iptables for TUN + masquerade. Build context = the revquic module root.
FROM golang:1.25-alpine AS build
ENV GOPROXY=direct GOFLAGS=-mod=mod CGO_ENABLED=0
RUN apk add --no-cache git
WORKDIR /src
COPY . .
RUN go build -o /out/revquic-exit ./cmd/revquic-exit

FROM alpine:3.20
RUN apk add --no-cache iproute2 iptables
COPY --from=build /out/revquic-exit /usr/local/bin/revquic-exit
ENTRYPOINT ["revquic-exit"]
