# revquic-client — Alpine + iputils/curl; entrypoint runs the relay smoke. Build context = revquic root.
FROM golang:1.25-alpine AS build
ENV GOPROXY=direct GOFLAGS=-mod=mod CGO_ENABLED=0
RUN apk add --no-cache git
WORKDIR /src
COPY . .
RUN go build -o /out/revquic-client ./cmd/revquic-client

FROM alpine:3.20
RUN apk add --no-cache iproute2 iptables iputils curl
COPY --from=build /out/revquic-client /usr/local/bin/revquic-client
COPY docker/client/smoke.sh /usr/local/bin/smoke.sh
RUN chmod +x /usr/local/bin/smoke.sh
ENTRYPOINT ["/usr/local/bin/smoke.sh"]
