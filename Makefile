.PHONY: generate check-generated test

generate:
	cd examples/echo && buf generate
	go run ./cmd/weaver generate ./...

check-generated: generate
	git diff --exit-code
	test -z "$$(git ls-files --others --exclude-standard)"

test:
	go test -race ./...
