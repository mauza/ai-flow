# agent-go plus headless Chromium, playwright-core and `browse`, so QA steps
# can load a page, tap through it, read console errors and save screenshots,
# and still build and run a Go backend (web-games QA needed both).
ARG BASE=ai-flow-agent-go:dev
FROM ${BASE}
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium fonts-liberation fonts-noto-color-emoji \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g --no-fund --no-audit playwright-core@1 \
 && npm cache clean --force
COPY deploy/images/browse.mjs /usr/local/lib/browse/browse.mjs
RUN printf '#!/bin/sh\nexec node /usr/local/lib/browse/browse.mjs "$@"\n' > /usr/local/bin/browse \
 && chmod 755 /usr/local/bin/browse
ENV CHROMIUM_PATH=/usr/bin/chromium \
    NODE_PATH=/usr/local/lib/node_modules
USER 1000:1000
