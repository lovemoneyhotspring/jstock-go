# 開発の入口。CI（.github/workflows/ci.yml）と同じ手順を `make ci` で回せる。
#
#   make build   実行ファイルを bin/ に作る（deploy/build.sh と同じ）
#   make test    テスト
#   make lint    go vet + staticcheck
#   make ci      build + lint + test（push 前に）

STATICCHECK := go run honnef.co/go/tools/cmd/staticcheck@2026.2

.PHONY: build test lint vet staticcheck ci fmt

build:
	deploy/build.sh

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

staticcheck:
	$(STATICCHECK) ./...

lint: vet staticcheck

fmt:
	gofmt -l -w cmd pkg

ci: fmt
	go build ./...
	$(MAKE) lint
	$(MAKE) test
