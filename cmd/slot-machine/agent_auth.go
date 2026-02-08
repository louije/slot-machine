package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (a *agentService) extractUser(r *http.Request) string {
	header := r.Header.Get("X-SlotMachine-User")
	switch a.authMode {
	case "hmac":
		idx := strings.LastIndex(header, ":")
		if idx < 1 {
			return ""
		}
		user, sig := header[:idx], header[idx+1:]
		mac := hmac.New(sha256.New, []byte(a.authSecret))
		mac.Write([]byte(user))
		expected := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			return ""
		}
		return user
	case "trusted":
		return header
	default:
		return ""
	}
}

func (a *agentService) buildSystemPrompt() string {
	var b strings.Builder
	b.WriteString("You are an AI assistant embedded in a web application via slot-machine.\n")
	b.WriteString("You are working in the application's source code directory.\n")

	agentMD, err := os.ReadFile(filepath.Join(a.stagingDir, "agent.md"))
	if err == nil && len(agentMD) > 0 {
		b.WriteString("\n")
		b.Write(agentMD)
		if agentMD[len(agentMD)-1] != '\n' {
			b.WriteString("\n")
		}
	}

	b.WriteString("\n## Conversation titling\n")
	b.WriteString("Include a conversation title in your responses using this format on its own line:\n")
	b.WriteString("[[TITLE: short descriptive title]]\n")
	b.WriteString("Include this in your first response. You may include it again to update the title if the topic changes.\n")

	return b.String()
}
