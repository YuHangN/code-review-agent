.PHONY: checker-image checker-image-refresh test

checker-image:
	docker build -f docker/checker.Dockerfile -t code-review-agent-checker:go1.26-staticcheck2026.1 .

checker-image-refresh:
	docker build --pull -f docker/checker.Dockerfile -t code-review-agent-checker:go1.26-staticcheck2026.1 .

test:
	go test ./...
