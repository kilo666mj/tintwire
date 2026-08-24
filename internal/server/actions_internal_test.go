package server

import "testing"

func TestParseBoundedIndexRejectsTrailingText(t *testing.T) {
	for _, value := range []string{"1abc", "-1", "101", ""} {
		if _, err := parseBoundedIndex(value); err == nil {
			t.Errorf("parseBoundedIndex(%q) succeeded", value)
		}
	}
	for _, value := range []string{"0", "1", "100"} {
		if _, err := parseBoundedIndex(value); err != nil {
			t.Errorf("parseBoundedIndex(%q): %v", value, err)
		}
	}
}
