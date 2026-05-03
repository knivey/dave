package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewScrollbarDefaults(t *testing.T) {
	tests := []struct {
		name       string
		cfg        TUIScrollbarConfig
		wantVis    bool
		wantAlways bool
	}{
		{
			name:       "all nil defaults to true/true",
			cfg:        TUIScrollbarConfig{},
			wantVis:    true,
			wantAlways: true,
		},
		{
			name:       "explicit true/true",
			cfg:        TUIScrollbarConfig{Visible: boolPtr(true), ShowAlways: boolPtr(true)},
			wantVis:    true,
			wantAlways: true,
		},
		{
			name:       "explicit false/false",
			cfg:        TUIScrollbarConfig{Visible: boolPtr(false), ShowAlways: boolPtr(false)},
			wantVis:    false,
			wantAlways: false,
		},
		{
			name:       "visible false show_always nil defaults show_always to true",
			cfg:        TUIScrollbarConfig{Visible: boolPtr(false)},
			wantVis:    false,
			wantAlways: true,
		},
		{
			name:       "visible nil show_always false defaults visible to true",
			cfg:        TUIScrollbarConfig{ShowAlways: boolPtr(false)},
			wantVis:    true,
			wantAlways: false,
		},
		{
			name:       "visible true show_always nil",
			cfg:        TUIScrollbarConfig{Visible: boolPtr(true)},
			wantVis:    true,
			wantAlways: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := NewScrollbar(tt.cfg)
			assert.Equal(t, tt.wantVis, sb.visible, "visible")
			assert.Equal(t, tt.wantAlways, sb.showAlways, "showAlways")
		})
	}
}
