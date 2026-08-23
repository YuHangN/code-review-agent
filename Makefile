.PHONY: checker-image test

checker-image:
	docker build --pull -f docker/checker.Dockerfile -t code-review-agent-checker:go1.26-staticcheck2026.1 .

test:
	go test ./...
