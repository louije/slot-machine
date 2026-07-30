package agent

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"
)

//go:embed static/chat.html
var chatHTML string

func (a *Service) handleChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(chatHTML))
}

func (a *Service) handleChatConfig(w http.ResponseWriter, r *http.Request) {
	title := a.chatTitle
	if title == "" {
		title = "slot-machine"
	}
	writeJSON(w, 200, map[string]string{
		"authMode":   a.authMode,
		"authSecret": a.authSecret,
		"chatTitle":  title,
		"chatAccent": a.chatAccent,
	})
}

func (a *Service) handleChatCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css")
	data, err := os.ReadFile(filepath.Join(a.workDir, "chat.css"))
	if err != nil {
		w.WriteHeader(200)
		return
	}
	w.Write(data)
}
