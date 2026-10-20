# Build context: the repository root (it needs e2e/contracts/MockERC20.bin).
FROM node:22-alpine
WORKDIR /app
COPY e2e/mockchain/package.json ./
RUN npm install --omit=dev --no-audit --no-fund
COPY e2e/mockchain/server.js ./
COPY e2e/contracts/MockERC20.bin ./MockERC20.bin
ENV PORT=8545 DATA_DIR=/data BLOCK_TIME_MS=2000 CHAIN_ID=31337 MOCKERC20_BIN=/app/MockERC20.bin
USER root
CMD ["node", "server.js"]
