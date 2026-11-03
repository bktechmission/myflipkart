# Ask Sauby · MyFlipKart.com
# Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
#
#   make setup   create .env with random secrets (once)
#   make dev     build the UI, start MockID (:9000) and Ask Sauby (:8080)
#   make eval    run the eval suites and release gates

-include .env
export

.PHONY: setup web idp run dev eval eval-llm gate vet clean

setup: .env
.env:
	./scripts/setup.sh

web:
	cd web && npm install --no-audit --no-fund && npx tsc -p .

idp: .env
	cd backend && DATA_DIR=../data go run ./cmd/mockidp

run: .env
	cd backend && DATA_DIR=../data WEB_DIR=../web/public go run ./cmd/server

dev: .env web
	@echo "Starting MockID on :9000 and Ask Sauby on http://localhost:8080 (Ctrl-C stops both)"
	cd backend && trap 'kill 0' EXIT INT TERM; \
	  DATA_DIR=../data go run ./cmd/mockidp & \
	  DATA_DIR=../data WEB_DIR=../web/public go run ./cmd/server

eval:
	cd backend && DATA_DIR=../data go run ./cmd/evals -label "$(or $(LABEL),manual)"

eval-llm:
	cd backend && DATA_DIR=../data go run ./cmd/evals -router llm -label "llm $(LABEL)"

gate:
	cd backend && DATA_DIR=../data go run ./cmd/evals -gate -label ci

vet:
	cd backend && gofmt -l . && go vet ./...

clean:
	rm -rf data web/public/dist web/node_modules
