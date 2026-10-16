package service

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestCompileHintOnlyForExternalDatabaseChannels(t *testing.T) {
	for _, ch := range []string{types.ChannelMySQL, types.ChannelPostgres} {
		hint := compileHintForChannel(ch)
		if hint == "" {
			t.Fatalf("channel %q: want a compile hint", ch)
		}
		if !strings.Contains(hint, "primary key") || !strings.Contains(hint, "inferred") {
			t.Fatalf("channel %q: hint misses the table-page instructions: %q", ch, hint)
		}
	}
	for _, ch := range []string{"", types.ChannelWeb, types.ChannelFeishu, types.ChannelConfluence} {
		if hint := compileHintForChannel(ch); hint != "" {
			t.Fatalf("channel %q: unexpected hint %q", ch, hint)
		}
	}
}

func TestIsExternalDBType(t *testing.T) {
	cases := map[string]bool{
		types.ConnectorTypeMySQL:    true,
		types.ConnectorTypePostgres: true,
		"feishu":                    false,
		"":                          false,
	}
	for dsType, want := range cases {
		if got := isExternalDBType(dsType); got != want {
			t.Fatalf("isExternalDBType(%q) = %v, want %v", dsType, got, want)
		}
	}
}
