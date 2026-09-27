# Runtime for pod nodes: node-runner (ai-flow node) + pi + the ai-flow pi extension,
# git, ripgrep, python3 and node. Runs as uid 1000 with a read-only root filesystem;
# everything writable lives under /work and /tmp.
FROM node:22-bookworm-slim
ARG PI_VERSION=0.73.1
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ripgrep python3 python-is-python3 python3-venv python3-pip ca-certificates bash jq curl less procps \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g --no-fund --no-audit @mariozechner/pi-coding-agent@${PI_VERSION} \
 && npm cache clean --force
COPY pi-ext /opt/ai-flow/pi-ext
COPY bin/linux/ai-flow /usr/local/bin/ai-flow
ENV AI_FLOW_PI=pi \
    AI_FLOW_PI_EXT=/opt/ai-flow/pi-ext/index.ts \
    HOME=/work/flow/home \
    PI_OFFLINE=1 \
    PYTHONDONTWRITEBYTECODE=1
USER 1000:1000
WORKDIR /work
ENTRYPOINT ["ai-flow"]
CMD ["node"]
