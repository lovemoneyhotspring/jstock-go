# 開発の入口。CI（.github/workflows/ci.yml）と同じ手順を `make ci` で回せる。
#
#   make build   実行ファイルを bin/ に作る（deploy/build.sh と同じ）
#   make test    テスト（パッケージを 1 つずつ。下の TESTFLAGS）
#   make lint    go vet + staticcheck
#   make fmt     gofmt で書き換える（作業ツリーが変わる）
#   make fmt-check  gofmt の差分があれば失敗する（書き換えない）
#   make ci      fmt-check + コンパイルの確認（go build ./...）+ lint + test（push 前に）
#
# **`make ci` は bin/ を作らない**（`go build ./...` は通るかを見るだけで、実行ファイルを残さない）。
# 本番の実行ファイルを入れ替えるのは `make build`（= deploy/build.sh）。ci が通っただけでは古い bin のまま。
#
# **`make ci` は作業ツリーを書き換えない**（整形は fmt-check で見るだけ）。8:42 の jstock-guard
# （deploy/guard-preopen.sh）は .go に未コミットの変更があると実行ファイルの作り直しも 1 世代前への
# 戻しもしないので、ci が gofmt -w で .go を書き換えると朝の自動復旧が止まる。直すのは `make fmt` を手で。
#
# 本番機（メモリ 11GB）で全パッケージを -race で並列に回すとメモリ不足で落ちる。既定は -p 1
# （ビルドもテストも 1 パッケージずつ）。速くしたいときは `make test TESTFLAGS='-race'` など。
# GitHub の CI は runner のメモリに余裕があるので並列のまま（.github/workflows/ci.yml）。
TESTFLAGS ?= -race -p 1

STATICCHECK := go run honnef.co/go/tools/cmd/staticcheck@2026.2

.PHONY: build test lint vet staticcheck ci fmt fmt-check

build:
	deploy/build.sh

test:
	go test $(TESTFLAGS) -count=1 ./...

vet:
	go vet ./...

staticcheck:
	$(STATICCHECK) ./...

lint: vet staticcheck

fmt:
	gofmt -l -w cmd pkg

fmt-check:
	@out=$$(gofmt -l cmd pkg); \
	if [ -n "$$out" ]; then echo "gofmt の差分があります（make fmt で直す）:"; echo "$$out"; exit 1; fi

ci: fmt-check
	go build ./...
	$(MAKE) lint
	$(MAKE) test
