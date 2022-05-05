FROM scratch
COPY hetzner-flatcar /
ENTRYPOINT ["/hetzner-flatcar"]
