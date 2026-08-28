FROM node:20-slim
WORKDIR /app
COPY package.json ./
COPY api ./api
COPY server.js ./
EXPOSE 3000
CMD ["node", "server.js"]
