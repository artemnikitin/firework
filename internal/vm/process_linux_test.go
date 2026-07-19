//go:build linux

package vm

import "testing"

func TestParseProcStartTicksHandlesSpacesAndParenthesesInCommand(t *testing.T) {
	stat := "123 (fire cracker (vm)) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242 20"
	start, err := parseProcStartTicks(stat)
	if err != nil {
		t.Fatal(err)
	}
	if start != 424242 {
		t.Fatalf("start ticks = %d, want 424242", start)
	}
}
