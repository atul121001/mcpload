FROM node:22-alpine
ENV NODE_ENV=production
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-fund --no-audit && npm cache clean --force
COPY server.mjs ./
USER node
EXPOSE 3000
# ENTRYPOINT, so extra args reach the server: `docker run -i --rm <image> --stdio [--persona blocking]`
ENTRYPOINT ["node", "server.mjs"]
