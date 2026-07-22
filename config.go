package main

import (
	"github.com/dorokuma/prism/internal/config"
)

type Config = config.Config
type AccountConfig = config.AccountConfig
type ConfigHolder = config.ConfigHolder

var LoadConfig = config.LoadConfig
var NewConfigHolder = config.NewConfigHolder
var ParseTrustedProxies = config.ParseTrustedProxies
var ReloadConfig = config.ReloadConfig

func init() {
	config.LogLevelHook = setLogLevel
}
