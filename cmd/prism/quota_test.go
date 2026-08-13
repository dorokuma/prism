package main

import (
	"bytes"
	"strings"
	"testing"
)

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
