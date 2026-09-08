# syntax=docker/dockerfile:1

# Admin UI (Next.js, EPIC-11 ADR-0073): its own Node service, separate from the
# ragctl binary/image. Build stage: install deps + `next build` (standalone output).
FROM node:24-bookworm-slim AS build
WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build

# Runtime stage: just the standalone server output + static assets, non-root
# (parity with the ragctl image, ADR-0002).
FROM node:24-bookworm-slim AS runtime
WORKDIR /app
ENV NODE_ENV=production PORT=3000 HOSTNAME=0.0.0.0

COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static

USER node
EXPOSE 3000
CMD ["node", "server.js"]
