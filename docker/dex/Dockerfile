# Dex IdP with the Revquic dev config baked in (named volumes/host mounts are awkward under colima).
FROM dexidp/dex:v2.39.1
COPY docker/dex/config.yaml /etc/dex/config.yaml
ENTRYPOINT ["dex", "serve", "/etc/dex/config.yaml"]
