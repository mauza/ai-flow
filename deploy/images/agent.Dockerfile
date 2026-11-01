# Runtime for pod nodes: node-runner (ai-flow node) + the agent harnesses (pi with
# the ai-flow pi extension, opencode, codex), git, ripgrep, python3 and node.
# Runs as uid 1000 with a read-only root filesystem; everything writable lives
# under /work and /tmp.
FROM node:22-bookworm-slim
ARG PI_VERSION=0.73.1
ARG CODEX_VERSION=0.166.0
# opencode 2.x ships as release binaries (npm's opencode-ai is the 1.x line).
ARG OPENCODE_VERSION=2.2.5
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ripgrep python3 python-is-python3 python3-venv python3-pip ca-certificates bash jq curl less procps \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g --no-fund --no-audit @mariozechner/pi-coding-agent@${PI_VERSION} @openai/codex@${CODEX_VERSION} \
 && npm cache clean --force \
 && arch=$(dpkg --print-architecture | sed 's/amd64/x64/') \
 && curl -fsSL "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-${arch}.tar.gz" \
    | tar -xz -C /usr/local/bin opencode \
 && opencode --version && codex --version
COPY pi-ext /opt/ai-flow/pi-ext
COPY bin/linux/ai-flow /usr/local/bin/ai-flow
ENV AI_FLOW_PI=pi \
    AI_FLOW_OPENCODE=opencode \
    AI_FLOW_CODEX=codex \
    AI_FLOW_PI_EXT=/opt/ai-flow/pi-ext/index.ts \
    HOME=/work/flow/home \
    PI_OFFLINE=1 \
    PYTHONDONTWRITEBYTECODE=1
USER 1000:1000
WORKDIR /work
ENTRYPOINT ["ai-flow"]
CMD ["node"]
