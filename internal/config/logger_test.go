package config

import "testing"

func TestNewLogger(t *testing.T) {
	if NewLogger("debug", "text") == nil {
		t.Fatal("text logger nil")
	}
	if NewLogger("info", "json") == nil {
		t.Fatal("json logger nil")
	}
}
