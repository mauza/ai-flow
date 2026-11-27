# Control plane: API + UI + engine + broker + Antigravity CLI (`agy`) for
# control-plane planner/assistant calls and llm-node completions on antigravity
# upstreams. Runs as nonroot uid 65532 with a read-only root filesystem.
FROM debian:bookworm-slim AS agy
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl bash \
 && curl -fsSL https://antigravity.google/cli/install.sh | bash -s -- --dir /usr/local/bin \
 && /usr/local/bin/agy --version

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 65532 nonroot \
 && useradd -u 65532 -g 65532 -M -d /tmp -s /usr/sbin/nologin nonroot
COPY --from=agy /usr/local/bin/agy /usr/local/bin/agy
COPY bin/linux/ai-flow /ai-flow
ENV AI_FLOW_AGY=agy
USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/ai-flow"]
CMD ["server", "-config", "/config"]
