# 開発の入口。CI（.github/workflows/ci.yml）と同じ手順を `make ci` で回せる。
#
#   make build   実行ファイルを bin/ に作る（deploy/build.sh と同じ）
#   make test    テスト
#   make lint    go vet + staticcheck
#   make ci      gofmt + コンパイルの確認（go build ./...）+ lint + test（push 前に）
#
# **`make ci` は bin/ を作らない**（`go build ./...` は通るかを見るだけで、実行ファイルを残さない）。
# 本番の実行ファイルを入れ替えるのは `make build`（= deploy/build.sh）。ci が通っただけでは古い bin のまま。

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
