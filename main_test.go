package main

import (
	"reflect"
	"testing"
)

func TestParseCSV(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"", nil},
		{"a,b,c", []string{"a", "b", "c"}},
		{" tenant-*, kube-system ,", []string{"tenant-*", "kube-system"}},
		// Malformed input
		{",,,", nil},
		{"   ", nil},
		{"a,,b", []string{"a", "b"}},
	}

	for _, tt := range tests {
		result := parseCSV(tt.input)
		if !reflect.DeepEqual(result, tt.expected) {
			t.Errorf("parseCSV(%q) = %v; want %v", tt.input, result, tt.expected)
		}
	}
}

func TestParseLabels(t *testing.T) {
	tests := []struct {
		input    string
		expected map[string]string
	}{
		{"", map[string]string{}},
		{"env=prod,tier=backend", map[string]string{"env": "prod", "tier": "backend"}},
		{"requires-offloading, tier=frontend , ", map[string]string{"requires-offloading": "", "tier": "frontend"}},
		// Malformed input
		{"=prod", map[string]string{}},
		{" = ,=", map[string]string{}},
		{",,,", map[string]string{}},
		{"env = prod", map[string]string{"env": "prod"}},
		{"env=", map[string]string{"env": ""}},
		{"env==prod", map[string]string{"env": "=prod"}},
		{"env=a,env=b", map[string]string{"env": "b"}},
	}

	for _, tt := range tests {
		result := parseLabels(tt.input)
		if !reflect.DeepEqual(result, tt.expected) {
			t.Errorf("parseLabels(%q) = %v; want %v", tt.input, result, tt.expected)
		}
	}
}
