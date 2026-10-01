//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// runUI для тестирования на Linux: открываем браузер (или нет) и блокируемся,
// чтобы сервер оставался живым и его можно было дёргать curl'ом.
func runUI(serverURL string) {
	if os.Getenv("CL_NOBROWSER") == "" {
		_ = exec.Command("xdg-open", serverURL).Start()
	}
	select {}
}
