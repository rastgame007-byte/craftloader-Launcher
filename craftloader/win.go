//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"

	webview "github.com/jchv/go-webview2"
)

// runUI открывает нативное окно приложения на WebView2
// (ритайм встроен в Windows 10/11; если вдруг его нет — открываем браузер).
func runUI(serverURL string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("webview2 недоступен (%v), открываю браузер", r)
			_ = exec.Command("cmd", "/c", "start", "", serverURL).Start()
			select {} // держим процесс живым
		}
	}()

	dataPath := filepath.Join(configDir(), "webview2")
	_ = os.MkdirAll(dataPath, 0o755)

	w := webview.NewWithOptions(webview.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  dataPath,
		WindowOptions: webview.WindowOptions{
			Title:  "CraftLoader 2.2 — моды, сборки и запуск Minecraft",
			Width:  1280,
			Height: 860,
			Center: true,
		},
	})
	defer w.Destroy()
	w.Navigate(serverURL)
	w.Run()
}
