BIN := agentium
VERSION ?= 0.6.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test bench install cross clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/agentium

test:
	go vet ./...
	go test -race ./...

bench: build
	./$(BIN) bench

install:
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/agentium

cross:
	@for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "$$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)-$$os-$$arch$$ext ./cmd/agentium || exit 1; \
	done

clean:
	rm -rf $(BIN) dist
