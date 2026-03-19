.PHONY: build run dev test clean migrate docker-up docker-down

# Build the server binary
build:
	go build -o bin/jukebox-server ./cmd/server

# Run the server
run: build
	./bin/jukebox-server

# Run with live reloading (requires air: go install github.com/air-verse/air@latest)
dev:
	air -c .air.toml || go run ./cmd/server

# Run tests
test:
	go test ./... -v

# Clean build artifacts
clean:
	rm -rf bin/

# Start Postgres + Redis via Docker Compose
docker-up:
	docker compose up -d

# Stop Docker services
docker-down:
	docker compose down

# Apply migrations manually
migrate:
	psql "$(DATABASE_URL)" -f migrations/001_initial.up.sql

# Reset database
migrate-down:
	psql "$(DATABASE_URL)" -f migrations/001_initial.down.sql

# Format and vet
lint:
	go fmt ./...
	go vet ./...
