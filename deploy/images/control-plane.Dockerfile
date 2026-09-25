# Control plane: API + UI + engine + broker. Built from a host-built static binary
# (make images), so the UI and Go build caches stay on the host.
FROM gcr.io/distroless/static-debian12:nonroot
COPY bin/linux/ai-flow /ai-flow
USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/ai-flow"]
CMD ["server", "-config", "/config"]
