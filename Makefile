.PHONY: build test race vet fmt check demo clean

build:
	go build -o bin/load-balancer ./cmd/load-balancer
	go build -o bin/backend ./cmd/backend

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Everything CI would run.
check: vet race
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

demo: build
	./scripts/run-local.sh

clean:
	rm -rf bin
