# NAT router for the multi-NAT testbed.
FROM alpine:3.20
RUN apk add --no-cache iproute2 iptables
COPY docker/nat/entrypoint.sh /usr/local/bin/nat-entrypoint.sh
RUN chmod +x /usr/local/bin/nat-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/nat-entrypoint.sh"]
