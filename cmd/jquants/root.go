package main

import (
	"path/filepath"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
)

var (
	appSettings = settings.LoadAppSettings()
	jquantsDir  = filepath.Join(appSettings.DataDir, "jquants")
)
