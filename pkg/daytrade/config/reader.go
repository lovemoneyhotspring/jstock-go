package config

import (
	"bytes"
	"io"
)

// newReader は go-toml のデコーダに渡す入力。
func newReader(raw []byte) io.Reader { return bytes.NewReader(raw) }
