# Kovio Backend

Go API gateway for the Kovio quiz game platform. Uses PostgreSQL, Redis, and Centrifugo.

## Quick start (Docker)

From this directory:

```bash
docker compose up -d --build
```

This builds and starts:
- Backend API on `localhost:8082`
- PostgreSQL on `localhost:5432`
- Redis on `localhost:6379`
- Centrifugo on `localhost:8000`

## Default admin

The backend seeds a default admin account on every boot:



Override via environment variables `SEED_ADMIN_EMAIL` and `SEED_ADMIN_PASSWORD` in `docker-compose.yml` or a `.env` file.

## Native development

If you prefer to run the backend outside Docker:

```bash
cd backend
cp .env.example .env
# edit .env with your local settings
go build -o api-gateway ./cmd/api-gateway
./run.sh
```

Remember to start PostgreSQL, Redis, and Centrifugo separately (or use the Docker Compose infra services).

## Project structure

```
backend/
├── cmd/api-gateway          # Application entry point
├── internal/
│   ├── handler              # HTTP handlers
│   ├── middleware           # Auth, RBAC, rate limiting
│   ├── model                # GORM entities
│   ├── db                   # Database connection & migrations
│   ├── cache                # Redis wrapper
│   ├── realtime             # Centrifugo integration
│   ├── pkg                  # JWT, license, audit helpers
│   └── cron                 # Background cleanup jobs
├── Dockerfile               # Production-ready Go image
├── docker-compose.yml       # Backend + infra services
├── centrifugo.json          # Centrifugo realtime config
├── run.sh                   # Helper script for native runs
└── .env.example             # Environment variable template
```
