FROM node:22-alpine
ENV NODE_ENV=production
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-fund --no-audit && npm cache clean --force
COPY server.mjs ./
USER node
EXPOSE 3000
CMD ["node", "server.mjs"]
