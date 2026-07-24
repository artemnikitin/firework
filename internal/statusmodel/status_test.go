package statusmodel

import (
	"strings"
	"testing"
)

func TestBoundedMessageSanitizesURLsAndControls(t *testing.T) {
	message := "download\nhttps://user:secret@example.com/image?token=secret#fragment failed " + strings.Repeat("x", 400)
	got := BoundedMessage(message)
	if strings.Contains(got, "secret") || strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("message was not sanitized: %q", got)
	}
	if len([]rune(got)) != MaxMessageLen {
		t.Fatalf("bounded length = %d, want %d", len([]rune(got)), MaxMessageLen)
	}
}
