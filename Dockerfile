FROM scratch

USER 65535:65534

COPY path-map /
ENTRYPOINT ["/path-map"]
