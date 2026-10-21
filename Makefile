.PHONY: build run fmt vet test check cross docker docker-run clean tidy

BIN      := loomy2api
VERSION  ?= dev
IMAGE    ?= loomy2api
LDFLAGS  := -s -w -X main.appVersion=$(VERSION)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/server

run: build
	./$(BIN) -config config.json

# 写回式格式化；检查口径与 CI 的 Verify gofmt（gofmt -l . 应无输出）一致
fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./... -count=1

# 本地提交前跑这一条：格式 + 静态检查 + 单测
check: fmt vet test
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "未格式化：$$out"; exit 1; fi

# 交叉编译冒烟：只验证可编译，不落产物
cross:
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o /dev/null ./cmd/server
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -o /dev/null ./cmd/server
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/server
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o /dev/null ./cmd/server

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

docker-run:
	docker compose up -d

clean:
	rm -f $(BIN) $(BIN).exe
	rm -rf dist

tidy:
	go mod tidy
