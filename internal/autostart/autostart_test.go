package autostart

import (
	"strings"
	"testing"
)

func TestRunValue(t *testing.T) {
	tests := []struct {
		name     string
		core     string
		data     string
		cfg      string
		wantCore string
		wantCfg  string
	}{
		{
			name:     "plain paths",
			core:     `C:\workbuddy2api\wb2api-core.exe`,
			data:     `C:\Users\User\.workbuddy2api`,
			cfg:      `C:\Users\User\.workbuddy2api\config.json`,
			wantCore: `C:\workbuddy2api\wb2api-core.exe`,
			wantCfg:  `C:\Users\User\.workbuddy2api\config.json`,
		},
		{
			name:     "paths with spaces",
			core:     `C:\Program Files\workbuddy2api\wb2api-core.exe`,
			data:     `C:\Users\John Doe\.workbuddy2api`,
			cfg:      `C:\Users\John Doe\.workbuddy2api\config.json`,
			wantCore: `C:\Program Files\workbuddy2api\wb2api-core.exe`,
			wantCfg:  `C:\Users\John Doe\.workbuddy2api\config.json`,
		},
		{
			name:     "paths with pre-existing quotes and extra spaces",
			core:     ` "C:\Program Files\workbuddy2api\wb2api-core.exe" `,
			data:     `"C:\Users\John Doe\.workbuddy2api"`,
			cfg:      `  "C:\Users\John Doe\.workbuddy2api\config.json" `,
			wantCore: `C:\Program Files\workbuddy2api\wb2api-core.exe`,
			wantCfg:  `C:\Users\John Doe\.workbuddy2api\config.json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RunValue(tt.core, tt.data, tt.cfg)
			want := "\"" + tt.wantCore + "\" -config \"" + tt.wantCfg + "\""
			if got != want {
				t.Fatalf("RunValue() = %q, want %q", got, want)
			}
			// Verify it never has duplicate quotes
			if strings.Contains(got, "\"\"") {
				t.Fatalf("RunValue() generated double quotes: %q", got)
			}
		})
	}
}
