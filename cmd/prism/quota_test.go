package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dorokuma/prism/internal/config"
)

func TestGrokPriceFor_AllZeroEffectiveTierIsMissing(t *testing.T) {
	cfg := &config.Config{
		ModelMetadata: config.ModelMetadataMap{
			"grok": {Cost: &config.ModelCost{
				Input:                1,
				LongContextThreshold: 100,
				LongContext:          &config.ModelCost{},
			}},
		},
	}
	priceFor := grokPriceFor(cfg)
	short := priceFor("grok-build", 99)
	if short == nil {
		t.Fatal("short-context tier must return a price")
	}
	if short.Input != 1 {
		t.Fatalf("short-context input price = %v, want 1", short.Input)
	}
	if got := priceFor("grok-build", 100); got != nil {
		t.Fatalf("all-zero effective long_context tier must return nil, got %+v", got)
	}
}
func TestRunQuotaHelp(t *testing.T) {
	err := runQuotaWith([]string{"-h"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("help: %v", err)
	}
}

func TestRunQuotaMissingConfig(t *testing.T) {
	origDir, origFB := usageConfigDir, usageConfigFallbackPath
	t.Cleanup(func() {
		usageConfigDir = origDir
		usageConfigFallbackPath = origFB
	})
	usageConfigDir = func() string { return t.TempDir() }
	usageConfigFallbackPath = t.TempDir() + "/no-such-config.yaml"
	var buf bytes.Buffer
	err := runQuotaWith(nil, &buf)
	if err == nil {
		t.Fatal("expected error when no config exists")
	}
	if !strings.Contains(err.Error(), "无法加载配置") {
		t.Fatalf("err = %v", err)
	}
}
