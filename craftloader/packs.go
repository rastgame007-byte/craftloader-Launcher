package main

// packs.go — свои сборки: создать, накидать модов, установить, запустить.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type PackMod struct {
	Source    string `json:"source"` // modrinth | cf
	Project   string `json:"project"`
	Title     string `json:"title"`
	Version   string `json:"version"`
	VersionID string `json:"version_id"`
	Filename  string `json:"filename"`
	URL       string `json:"url"`
	Icon      string `json:"icon"`
}

type CustomPack struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	MC      string    `json:"mc"`
	Loader  string    `json:"loader"`
	Mods    []PackMod `json:"mods"`
	Created string    `json:"created"`
}

func packByID(id string) *CustomPack {
	for i := range config.Packs {
		if config.Packs[i].ID == id {
			return &config.Packs[i]
		}
	}
	return nil
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
}

// GET /api/packs | POST /api/packs {action: create|delete|addmod|removemod|frommods|setmeta}
func handlePacks(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var req struct {
			Action string  `json:"action"`
			ID     string  `json:"id"`
			Name   string  `json:"name"`
			MC     string  `json:"mc"`
			Loader string  `json:"loader"`
			Mod    PackMod `json:"mod"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad request"})
			return
		}
		switch req.Action {
		case "create":
			name := sanitizeName(req.Name)
			if name == "" {
				writeJSON(w, 400, map[string]string{"error": "Введи название сборки"})
				return
			}
			p := CustomPack{
				ID: strconv.FormatInt(time.Now().UnixNano(), 36), Name: name,
				MC: req.MC, Loader: req.Loader, Mods: []PackMod{}, Created: time.Now().Format("2006-01-02"),
			}
			config.Packs = append(config.Packs, p)
			saveConfig()
			writeJSON(w, 200, map[string]interface{}{"ok": true, "pack": p})
		case "delete":
			out := []CustomPack{}
			for _, p := range config.Packs {
				if p.ID != req.ID {
					out = append(out, p)
				}
			}
			config.Packs = out
			saveConfig()
			writeJSON(w, 200, map[string]interface{}{"ok": true})
		case "setmeta":
			p := packByID(req.ID)
			if p == nil {
				writeJSON(w, 404, map[string]string{"error": "Сборка не найдена"})
				return
			}
			if req.Name != "" {
				p.Name = sanitizeName(req.Name)
			}
			if req.MC != "" {
				p.MC = req.MC
			}
			if req.Loader != "" {
				p.Loader = req.Loader
			}
			saveConfig()
			writeJSON(w, 200, map[string]interface{}{"ok": true})
		case "addmod":
			p := packByID(req.ID)
			if p == nil {
				writeJSON(w, 404, map[string]string{"error": "Сборка не найдена"})
				return
			}
			if req.Mod.Filename == "" && req.Mod.URL == "" {
				writeJSON(w, 400, map[string]string{"error": "Нет данных о версии"})
				return
			}
			// не дублируем
			for _, m := range p.Mods {
				if m.Filename == req.Mod.Filename && req.Mod.Filename != "" {
					writeJSON(w, 200, map[string]interface{}{"ok": true, "dup": true})
					return
				}
			}
			p.Mods = append(p.Mods, req.Mod)
			saveConfig()
			writeJSON(w, 200, map[string]interface{}{"ok": true, "count": len(p.Mods)})
		case "removemod":
			p := packByID(req.ID)
			if p == nil {
				writeJSON(w, 404, map[string]string{"error": "Сборка не найдена"})
				return
			}
			out := []PackMod{}
			for _, m := range p.Mods {
				if m.Filename != req.Mod.Filename || m.VersionID != req.Mod.VersionID {
					out = append(out, m)
				}
			}
			p.Mods = out
			saveConfig()
			writeJSON(w, 200, map[string]interface{}{"ok": true, "count": len(p.Mods)})
		case "frommods":
			// собрать сборку из того, что уже скачано в папку mods
			name := sanitizeName(req.Name)
			if name == "" {
				name = "Моя сборка"
			}
			m := loadManifest()
			if len(m.Files) == 0 {
				writeJSON(w, 400, map[string]string{"error": "Папка mods пуста (или моды качались не через CraftLoader)"})
				return
			}
			mcCount := map[string]int{}
			p := CustomPack{
				ID: strconv.FormatInt(time.Now().UnixNano(), 36), Name: name,
				Loader: req.Loader, Mods: []PackMod{}, Created: time.Now().Format("2006-01-02"),
			}
			for fname, ref := range m.Files {
				p.Mods = append(p.Mods, PackMod{
					Source: ref.Source, Project: ref.Project, Title: ref.Title,
					Version: ref.Version, VersionID: ref.VersionID, Filename: fname, URL: "",
				})
				if ref.MC != "" {
					mcCount[ref.MC]++
				}
			}
			if req.MC != "" {
				p.MC = req.MC
			} else {
				best := ""
				bestN := 0
				for k, n := range mcCount {
					if n > bestN {
						best, bestN = k, n
					}
				}
				p.MC = best
			}
			config.Packs = append(config.Packs, p)
			saveConfig()
			writeJSON(w, 200, map[string]interface{}{"ok": true, "pack": p})
		default:
			writeJSON(w, 400, map[string]string{"error": "unknown action"})
		}
		return
	}
	writeJSON(w, 200, map[string]interface{}{"packs": config.Packs})
}

// resolvePackMod: если URL неизвестен — ищем версию по version_id
func resolvePackMod(m PackMod) (string, string, error) {
	if m.URL != "" && m.Filename != "" {
		return m.URL, m.Filename, nil
	}
	if m.Source == "modrinth" {
		body, code := modrinthGet("https://api.modrinth.com/v2/project/" + m.Project + "/version")
		if code == 200 {
			var vers []struct {
				ID            string `json:"id"`
				VersionNumber string `json:"version_number"`
				Files         []struct {
					URL      string `json:"url"`
					Filename string `json:"filename"`
					Primary  bool   `json:"primary"`
				} `json:"files"`
			}
			if json.Unmarshal(body, &vers) == nil {
				var pick *struct {
					ID            string `json:"id"`
					VersionNumber string `json:"version_number"`
					Files         []struct {
						URL      string `json:"url"`
						Filename string `json:"filename"`
						Primary  bool   `json:"primary"`
					} `json:"files"`
				}
				for i := range vers {
					if m.VersionID == "" || vers[i].ID == m.VersionID {
						pick = &vers[i]
						break
					}
				}
				if pick == nil && len(vers) > 0 {
					pick = &vers[0]
				}
				if pick != nil {
					for _, f := range pick.Files {
						if f.Primary {
							return f.URL, f.Filename, nil
						}
					}
					if len(pick.Files) > 0 {
						return pick.Files[0].URL, pick.Files[0].Filename, nil
					}
				}
			}
		}
		return "", "", fmt.Errorf("не нашёл версию %s на Modrinth", m.Title)
	}
	if m.Source == "cf" {
		body, code := cfwidgetFetch("minecraft/mc-mods/" + m.Project)
		if code == 200 {
			if proj, err := cfNormalize(body, m.Project); err == nil {
				var pick *normVersion
				for i := range proj.Versions {
					vid := strconv.FormatInt(proj.Versions[i].FileID, 10)
					if m.VersionID == "" || vid == m.VersionID {
						pick = &proj.Versions[i]
						break
					}
				}
				if pick == nil && len(proj.Versions) > 0 {
					pick = &proj.Versions[0]
				}
				if pick != nil && len(pick.Files) > 0 {
					return pick.Files[0].URL, pick.Files[0].Filename, nil
				}
			}
		}
		return "", "", fmt.Errorf("не нашёл версию %s на CurseForge", m.Title)
	}
	return "", "", fmt.Errorf("неизвестный источник")
}

// POST /api/packs/install {id, dest: instance|mods}
func handlePackInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	if !opMutex.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "Занято"})
		return
	}
	defer opMutex.Unlock()

	var req struct {
		ID   string `json:"id"`
		Dest string `json:"dest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	p := packByID(req.ID)
	if p == nil {
		writeJSON(w, 404, map[string]string{"error": "Сборка не найдена"})
		return
	}
	var target string
	if req.Dest == "mods" {
		mods := effectiveModsPath()
		if mods == "" {
			writeJSON(w, 400, map[string]string{"error": "no_path"})
			return
		}
		target = mods
	} else {
		target = filepath.Join(instancesDir(), sanitizeName(p.Name))
	}
	_ = os.MkdirAll(target, 0o755)
	// Fabric/Quilt ищут моды в подпапке mods/
	modDir := target
	if req.Dest != "mods" {
		modDir = filepath.Join(target, "mods")
		_ = os.MkdirAll(modDir, 0o755)
	}

	progReset("install", p.Name, int64(len(p.Mods)))
	defer progActive.Store(false)

	installed, skipped, failed := []string{}, []string{}, []string{}
	seen := map[string]bool{}
	for i, m := range p.Mods {
		progItemsDone.Store(int64(i))
		progLabel.Store(m.Title)
		u, fn, err := resolvePackMod(m)
		if err != nil {
			skipped = append(skipped, m.Title+" ("+err.Error()+")")
			continue
		}
		if seen[fn] {
			continue
		}
		seen[fn] = true
		if err := downloadToFileQuiet(u, filepath.Join(modDir, fn)); err != nil {
			failed = append(failed, fn+" ("+err.Error()+")")
			continue
		}
		installed = append(installed, fn)
	}
	progItemsDone.Store(int64(len(p.Mods)))

	// метаданные инстанса
	meta := map[string]interface{}{
		"name": p.Name, "format": "craftloader-pack", "mc": p.MC, "loader": p.Loader,
		"installed": time.Now().Format("2006-01-02 15:04"), "files": len(installed),
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	_ = os.WriteFile(filepath.Join(target, "craftloader-instance.json"), data, 0o644)

	writeJSON(w, 200, map[string]interface{}{
		"ok": true, "dir": target, "installed": installed, "skipped": skipped, "failed": failed,
	})
}
