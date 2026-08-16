FROM node:22-bookworm-slim

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg fonts-dejavu-core ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g pnpm@10.32.0

RUN printf 'onlyBuiltDependencies:\n  - node-datachannel\n' > pnpm-workspace.yaml \
    && printf '{"private":true}\n' > package.json \
    && pnpm add webtorrent@3.0.21

COPY server.mjs /app/server.mjs

ENTRYPOINT ["node", "/app/server.mjs"]
