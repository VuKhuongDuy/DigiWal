# docker-compose.yml pulls in ../../infra/docker-compose.yml via `include:` — don't also pass it with -f.
COMPOSE = docker compose

.PHONY: up down logs test

up:
	$(COMPOSE) up -d --build history-service
	@until curl -sf http://localhost:8083/healthz > /dev/null; do sleep 1; done
	@echo "up: http://localhost:8083"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f history-service

# The MongoDB tests need a real MongoDB: TEST_MONGO_URI=mongodb://custody:custody@localhost:27017/?authSource=admin
# (`docker compose -f ../../infra/docker-compose.yml up -d mongo`). They skip without it.
test:
	go test ./...
