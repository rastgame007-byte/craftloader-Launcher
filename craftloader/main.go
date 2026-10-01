package main

import (
	"archive/zip"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed ui
var uiFS embed.FS

const appVersion = "2.2"
const userAgent = "CraftLoader/" + appVersion + " (personal mod manager)"
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 CraftLoader/" + appVersion

var apiClient = &http.Client{Timeout: 25 * time.Second}
var dlClient = &http.Client{Timeout: 15 * time.Minute}

// ============================ конфиг ============================

type Favorite struct {
	Source string `json:"source"` // modrinth | cf
	Slug   string `json:"slug"`
	Title  string `json:"title"`
	Icon   string `json:"icon"`
}

type Config struct {
	ModsPath  string       `json:"mods_path"`
	Favorites []Favorite   `json:"favorites"`
	Packs     []CustomPack `json:"packs"`
	Nickname  string       `json:"nickname"`
}

var (
	cfgMutex sync.Mutex
	config   Config
	cfgPath  string
)

func configDir() string {
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "CraftLoader")
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		return filepath.Join(home, ".config", "craftloader")
	}
	return "."
}

func instancesDir() string { return filepath.Join(configDir(), "instances") }

func loadConfig() {
	cfgPath = filepath.Join(configDir(), "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	var c Config
	if json.Unmarshal(data, &c) == nil {
		config = c
	} else {
		// конфиг повреждён (например, запись прервали) — сохраняем копию и стартуем с чистого
		_ = os.WriteFile(cfgPath+".bad", data, 0o644)
		log.Printf("craftloader: конфиг повреждён, копия в %s.bad, стартую с пустого", cfgPath)
	}
}

func saveConfig() {
	cfgMutex.Lock()
	defer cfgMutex.Unlock()
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0o755)
	data, _ := json.MarshalIndent(config, "", "  ")
	// атомарная запись: tmp + rename, чтобы не осталось обрезанного файла
	tmp := cfgPath + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, cfgPath)
	} else {
		_ = os.WriteFile(cfgPath, data, 0o644)
	}
}

// ============================ папка mods ============================

func modsCandidates() []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			add(filepath.Join(appdata, ".minecraft", "mods"))
		}
		if ud := os.Getenv("USERPROFILE"); ud != "" {
			add(filepath.Join(ud, "AppData", "Roaming", ".minecraft", "mods"))
			add(filepath.Join(ud, ".minecraft", "mods"))
		}
		if ld := os.Getenv("LOCALAPPDATA"); ld != "" {
			add(filepath.Join(ld, "PrismLauncher", "minecraft", "mods"))
		}
	} else {
		home, _ := os.UserHomeDir()
		add(filepath.Join(home, ".minecraft", "mods"))
	}
	return out
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// ensureModsPath: если папки mods нет — создаём сами, чтобы скачивание
// работало сразу, без вопросов пользователю (сменить можно в настройках)
func ensureModsPath() {
	if effectiveModsPath() != "" {
		return
	}
	p := ""
	for _, c := range modsCandidates() {
		if dirExists(filepath.Dir(c)) { // есть .minecraft / Prism — создаём mods рядом
			p = c
			break
		}
	}
	if p == "" {
		p = filepath.Join(configDir(), "mods")
	}
	if err := os.MkdirAll(p, 0o755); err == nil {
		config.ModsPath = p
		saveConfig()
		log.Printf("craftloader: папка модов: %s", p)
	}
}

func effectiveModsPath() string {
	if config.ModsPath != "" {
		return config.ModsPath
	}
	for _, c := range modsCandidates() {
		if dirExists(c) {
			return c
		}
	}
	return ""
}

// ============================ прогресс ============================

var (
	progActive     atomic.Bool
	progPhase      atomic.Value // string
	progLabel      atomic.Value // string
	progCur        atomic.Int64
	progTotal      atomic.Int64
	progItemsDone  atomic.Int64
	progItemsTotal atomic.Int64
	opMutex        sync.Mutex // сериализуем скачивания/установки
)

func progReset(phase, label string, items int64) {
	progPhase.Store(phase)
	progLabel.Store(label)
	progCur.Store(0)
	progTotal.Store(0)
	progItemsDone.Store(0)
	progItemsTotal.Store(items)
	progActive.Store(true)
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// ============================ helpers ============================

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRawJSON(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func httpGet(rawurl, ua string) ([]byte, int) {
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return []byte(`{"error":"bad request"}`), 400
	}
	req.Header.Set("User-Agent", ua)
	resp, err := apiClient.Do(req)
	if err != nil {
		log.Printf("http: %v", err)
		return []byte(`{"error":"Сеть недоступна. Проверь интернет."}`), 502
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return body, resp.StatusCode
}

func modrinthGet(rawurl string) ([]byte, int) { return httpGet(rawurl, userAgent) }

func downloadAllowed(u *url.URL) bool {
	h := strings.ToLower(u.Host)
	if h == "modrinth.com" || strings.HasSuffix(h, ".modrinth.com") {
		return true
	}
	if strings.HasSuffix(h, ".forgecdn.net") {
		return true
	}
	if h == "www.curseforge.com" || h == "curseforge.com" {
		return strings.HasPrefix(u.Path, "/api/v1/")
	}
	if h == "github.com" || h == "raw.githubusercontent.com" || h == "objects.githubusercontent.com" {
		return true
	}
	for _, mh := range []string{
		"launchermeta.mojang.com", "piston-meta.mojang.com", "piston-data.mojang.com",
		"resources.download.minecraft.net", "libraries.minecraft.net",
		"meta.fabricmc.net", "maven.fabricmc.net",
		"meta.quiltmc.org", "maven.quiltmc.org",
	} {
		if h == mh {
			return true
		}
	}
	return false
}

// downloadToFile качает url в dest с учётом прогресса
func downloadToFile(rawurl, dest string) error {
	u, err := url.Parse(rawurl)
	if err != nil || !downloadAllowed(u) {
		return fmt.Errorf("источник не разрешён: %s", rawurl)
	}
	req, _ := http.NewRequest("GET", u.String(), nil)
	req.Header.Set("User-Agent", browserUA)
	resp, err := dlClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("сервер вернул %d", resp.StatusCode)
	}
	progCur.Store(0)
	progTotal.Store(resp.ContentLength)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, &countingReader{r: resp.Body, n: &progCur})
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("скачивание прервано")
	}
	return os.Rename(tmp, dest)
}

func safeJoin(base, rel string) (string, bool) {
	rel = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(rel)), "./")
	if rel == "" || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return "", false
	}
	return filepath.Join(base, filepath.FromSlash(rel)), true
}

func openInSystem(path string) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("explorer", path)
	} else {
		cmd = exec.Command("xdg-open", path)
	}
	_ = cmd.Start()
}

// ============================ манифест скачанных модов ============================

type ModRef struct {
	Source    string `json:"source"`
	Project   string `json:"project"`
	Title     string `json:"title"`
	Version   string `json:"version"`
	VersionID string `json:"version_id"`
	MC        string `json:"mc,omitempty"`
	Added     string `json:"added"`
}

type Manifest struct {
	Files map[string]ModRef `json:"files"`
}

func manifestPath() string {
	m := effectiveModsPath()
	if m == "" {
		return ""
	}
	return filepath.Join(m, ".craftloader.json")
}

func loadManifest() Manifest {
	var m Manifest
	p := manifestPath()
	if p == "" {
		return Manifest{Files: map[string]ModRef{}}
	}
	data, err := os.ReadFile(p)
	if err == nil {
		_ = json.Unmarshal(data, &m)
	}
	if m.Files == nil {
		m.Files = map[string]ModRef{}
	}
	return m
}

func saveManifest(m Manifest) {
	p := manifestPath()
	if p == "" {
		return
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(p, data, 0o644)
}

// ============================ CFWidget (CurseForge) ============================

type cfCacheEntry struct {
	body []byte
	at   time.Time
}

var (
	cfCacheMu sync.Mutex
	cfCache   = map[string]cfCacheEntry{}
	cfLast    time.Time
)

func cfwidgetFetch(path string) ([]byte, int) {
	cfCacheMu.Lock()
	if e, ok := cfCache[path]; ok && time.Since(e.at) < 10*time.Minute {
		cfCacheMu.Unlock()
		return e.body, 200
	}
	// бережный rate-limit между обращениями
	if d := 900*time.Millisecond - time.Since(cfLast); d > 0 {
		time.Sleep(d)
	}
	cfLast = time.Now()
	cfCacheMu.Unlock()

	for attempt := 0; attempt < 3; attempt++ {
		body, code := httpGet("https://api.cfwidget.com/"+path, browserUA)
		if code == 202 || code == 429 { // проект в очереди кэширования
			time.Sleep(3 * time.Second)
			continue
		}
		if code == 200 {
			cfCacheMu.Lock()
			cfCache[path] = cfCacheEntry{body: body, at: time.Now()}
			cfCacheMu.Unlock()
		}
		return body, code
	}
	return []byte(`{"error":"CurseForge не отвечает, попробуй позже"}`), 502
}

var knownLoaders = map[string]bool{"fabric": true, "forge": true, "quilt": true, "neoforge": true, "neoforge-legacy": true, "rift": true, "liteloader": true}

type cfFileRaw struct {
	ID         int64    `json:"id"`
	Display    string   `json:"display"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Filesize   int64    `json:"filesize"`
	UploadedAt string   `json:"uploaded_at"`
	Versions   []string `json:"versions"`
}

type normFile struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Primary  bool   `json:"primary"`
}

type normVersion struct {
	VersionNumber string     `json:"version_number"`
	VersionType   string     `json:"version_type"`
	DatePublished string     `json:"date_published"`
	Loaders       []string   `json:"loaders"`
	GameVersions  []string   `json:"game_versions"`
	FileID        int64      `json:"file_id"`
	Downloads     int64      `json:"downloads"`
	Files         []normFile `json:"files"`
}

type cfProject struct {
	Source      string        `json:"source"`
	Slug        string        `json:"slug"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	IconURL     string        `json:"icon_url"`
	Downloads   int64         `json:"downloads"`
	Updated     string        `json:"updated"`
	PageURL     string        `json:"page_url"`
	Versions    []normVersion `json:"versions"`
}

func cfNormalize(body []byte, projectID string) (*cfProject, error) {
	var raw struct {
		ID        int64  `json:"id"`
		Title     string `json:"title"`
		Summary   string `json:"summary"`
		Thumbnail string `json:"thumbnail"`
		Downloads struct {
			Monthly int64 `json:"monthly"`
			Total   int64 `json:"total"`
		} `json:"downloads"`
		CreatedAt string      `json:"created_at"`
		Files     []cfFileRaw `json:"files"`
		URLs      struct {
			Curseforge string `json:"curseforge"`
		} `json:"urls"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	pid := projectID
	if pid == "" {
		pid = strconv.FormatInt(raw.ID, 10)
	}
	versions := []normVersion{}
	for _, f := range raw.Files {
		loaders := []string{}
		gvs := []string{}
		for _, v := range f.Versions {
			lv := strings.ToLower(v)
			if knownLoaders[lv] {
				loaders = append(loaders, lv)
			} else if v != "Client" && v != "Server" {
				gvs = append(gvs, v)
			}
		}
		vt := f.Type
		if vt != "release" && vt != "beta" && vt != "alpha" {
			vt = "release"
		}
		versions = append(versions, normVersion{
			VersionNumber: f.Display,
			VersionType:   vt,
			DatePublished: f.UploadedAt,
			Loaders:       loaders,
			GameVersions:  gvs,
			FileID:        f.ID,
			Files: []normFile{{
				URL:      "https://www.curseforge.com/api/v1/mods/" + pid + "/files/" + strconv.FormatInt(f.ID, 10) + "/download",
				Filename: f.Name,
				Size:     f.Filesize,
				Primary:  true,
			}},
		})
	}
	return &cfProject{
		Source:      "cf",
		Slug:        pid,
		Title:       raw.Title,
		Description: raw.Summary,
		IconURL:     raw.Thumbnail,
		Downloads:   raw.Downloads.Total,
		Updated:     raw.CreatedAt,
		PageURL:     raw.URLs.Curseforge,
		Versions:    versions,
	}, nil
}

// parseCFInput: ссылка/слаг/ID -> путь cfwidget
func parseCFInput(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("пусто")
	}
	if strings.Contains(input, "curseforge.com") {
		u, err := url.Parse(input)
		if err != nil {
			return "", fmt.Errorf("кривая ссылка")
		}
		segs := []string{}
		for _, s := range strings.Split(u.Path, "/") {
			if s != "" {
				segs = append(segs, s)
			}
		}
		// minecraft/mc-mods/jei  |  minecraft/modpacks/rlcraft | projects/123
		if len(segs) >= 3 && segs[0] == "minecraft" {
			return strings.Join(segs[:3], "/"), nil
		}
		if len(segs) >= 2 && segs[0] == "projects" {
			return "projects/" + segs[1], nil
		}
		if len(segs) >= 1 && len(segs) <= 2 {
			return "minecraft/mc-mods/" + segs[len(segs)-1], nil
		}
		return "", fmt.Errorf("не понял ссылку")
	}
	if regexp.MustCompile(`^\d+$`).MatchString(input) {
		return "minecraft/mc-mods/" + input, nil
	}
	// попробуем слаг как мод, затем как модпак
	return "minecraft/mc-mods/" + input, nil
}

// ============================ теги версий MC ============================

var (
	tagsMu       sync.Mutex
	tagsVersions []string
	tagsAt       time.Time
)

var versionRe = regexp.MustCompile(`^\d+(\.\d+)*$`)

var fallbackVersions = []string{
	"1.21.4", "1.21.3", "1.21.1", "1.21", "1.20.6", "1.20.4", "1.20.1", "1.20",
	"1.19.4", "1.19.2", "1.19", "1.18.2", "1.17.1", "1.16.5", "1.12.2", "1.8.9",
}

func fetchGameVersions() []string {
	tagsMu.Lock()
	defer tagsMu.Unlock()
	if tagsVersions != nil && time.Since(tagsAt) < 6*time.Hour {
		return tagsVersions
	}
	body, code := modrinthGet("https://api.modrinth.com/v2/tag/game_version")
	if code == 200 {
		var raw []struct {
			Version     string `json:"version"`
			VersionType string `json:"version_type"`
		}
		if json.Unmarshal(body, &raw) == nil {
			out := []string{}
			for _, v := range raw {
				if v.VersionType == "release" && versionRe.MatchString(v.Version) {
					out = append(out, v.Version)
				}
			}
			if len(out) > 0 {
				if len(out) > 80 {
					out = out[:80]
				}
				tagsVersions = out
				tagsAt = time.Now()
				return out
			}
		}
	}
	return fallbackVersions
}

// ============================ готовые мини-сборки ============================

type CuratedPack struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Desc string   `json:"desc"`
	Icon string   `json:"icon"`
	Mods []string `json:"mods"`
}

var curatedPacks = []CuratedPack{
	{
		ID:   "opt",
		Name: "⚡ Минимальная оптимизация",
		Desc: "Только перфоманс-моды без лишнего: FPS выше, ничего не меняется в игре. Fabric или NeoForge.",
		Icon: "⚡",
		Mods: []string{"sodium", "lithium", "ferrite-core", "immediatelyfast", "entityculling", "dynamic-fps", "krypton", "sodium-extra"},
	},
}

// ============================ обработчики ============================

// /api/search?q=&loader=&mc=&index=&type=mod|modpack
func handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	loader := r.URL.Query().Get("loader")
	mc := r.URL.Query().Get("mc")
	cat := r.URL.Query().Get("cat")
	index := r.URL.Query().Get("index")
	offset := r.URL.Query().Get("offset")
	ptype := r.URL.Query().Get("type")
	if index == "" {
		index = "relevance"
	}
	if ptype == "" {
		ptype = "mod"
	}
	if offset == "" {
		offset = "0"
	}

	facets := [][]string{{"project_type:" + ptype}}
	if loader != "" {
		facets = append(facets, []string{"categories:" + loader})
	}
	if mc != "" {
		facets = append(facets, []string{"versions:" + mc})
	}
	if cat != "" {
		facets = append(facets, []string{"categories:" + cat})
	}
	fj, _ := json.Marshal(facets)

	v := url.Values{}
	v.Set("query", q)
	v.Set("facets", string(fj))
	v.Set("index", index)
	v.Set("offset", offset)
	v.Set("limit", "40")

	body, code := modrinthGet("https://api.modrinth.com/v2/search?" + v.Encode())
	writeRawJSON(w, code, body)
}

// /api/project?slug= (modrinth)
func handleProject(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" || strings.ContainsAny(slug, "/?#") {
		writeJSON(w, 400, map[string]string{"error": "bad slug"})
		return
	}
	body, code := modrinthGet("https://api.modrinth.com/v2/project/" + slug)
	writeRawJSON(w, code, body)
}

// /api/versions?slug=&loader=&mc= (modrinth)
func handleVersions(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	loader := r.URL.Query().Get("loader")
	mc := r.URL.Query().Get("mc")
	if slug == "" || strings.ContainsAny(slug, "/?#") {
		writeJSON(w, 400, map[string]string{"error": "bad slug"})
		return
	}
	v := url.Values{}
	if loader != "" {
		lj, _ := json.Marshal([]string{loader})
		v.Set("loaders", string(lj))
	}
	if mc != "" {
		mj, _ := json.Marshal([]string{mc})
		v.Set("game_versions", string(mj))
	}
	u := "https://api.modrinth.com/v2/project/" + slug + "/version"
	if enc := v.Encode(); enc != "" {
		u += "?" + enc
	}
	body, code := modrinthGet(u)
	writeRawJSON(w, code, body)
}

// /api/cf/project?input= — проект CurseForge по ссылке/слагу/ID
func handleCFProject(w http.ResponseWriter, r *http.Request) {
	input := strings.TrimSpace(r.URL.Query().Get("input"))
	path, err := parseCFInput(input)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	body, code := cfwidgetFetch(path)
	if code == 404 && !strings.Contains(input, "curseforge.com") && !regexp.MustCompile(`^\d+$`).MatchString(input) {
		// возможно это модпак
		body, code = cfwidgetFetch("minecraft/modpacks/" + input)
	}
	if code != 200 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			writeJSON(w, code, map[string]string{"error": e.Error})
		} else {
			writeJSON(w, code, map[string]string{"error": "Не нашёл такой проект на CurseForge"})
		}
		return
	}
	proj, err := cfNormalize(body, "")
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "CurseForge вернул мусор"})
		return
	}
	writeJSON(w, 200, proj)
}

// /api/download POST {url, filename, source, project, title, version_id, version, mc}
func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	if !opMutex.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "Уже что-то качается, подожди"})
		return
	}
	defer opMutex.Unlock()

	var req struct {
		URL       string `json:"url"`
		Filename  string `json:"filename"`
		Source    string `json:"source"`
		Project   string `json:"project"`
		Title     string `json:"title"`
		VersionID string `json:"version_id"`
		Version   string `json:"version"`
		MC        string `json:"mc"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}

	mods := effectiveModsPath()
	if mods == "" {
		writeJSON(w, 400, map[string]string{"error": "no_path"})
		return
	}
	if err := os.MkdirAll(mods, 0o755); err != nil {
		writeJSON(w, 500, map[string]string{"error": "Не могу создать папку mods"})
		return
	}
	fname := filepath.Base(req.Filename)
	if fname == "" || fname == "." || fname == "/" || fname == `\` {
		writeJSON(w, 400, map[string]string{"error": "bad filename"})
		return
	}

	progReset("download", fname, 1)
	defer progActive.Store(false)

	dest := filepath.Join(mods, fname)
	if err := downloadToFile(req.URL, dest); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}

	// запись в манифест для проверки обновлений
	if req.Source != "" && req.Project != "" {
		m := loadManifest()
		m.Files[fname] = ModRef{
			Source: req.Source, Project: req.Project, Title: req.Title,
			Version: req.Version, VersionID: req.VersionID, MC: req.MC,
			Added: time.Now().Format("2006-01-02"),
		}
		saveManifest(m)
	}
	log.Printf("скачано: %s -> %s", fname, dest)
	writeJSON(w, 200, map[string]interface{}{"ok": true, "path": dest})
}

// /api/progress
func handleProgress(w http.ResponseWriter, r *http.Request) {
	label, _ := progLabel.Load().(string)
	phase, _ := progPhase.Load().(string)
	total, cur := progTotal.Load(), progCur.Load()
	pct := 0.0
	if total > 0 {
		pct = float64(cur) / float64(total) * 100
	}
	writeJSON(w, 200, map[string]interface{}{
		"active":      progActive.Load(),
		"phase":       phase,
		"label":       label,
		"current":     cur,
		"total":       total,
		"percent":     pct,
		"items_done":  progItemsDone.Load(),
		"items_total": progItemsTotal.Load(),
	})
}

// /api/config
func handleConfig(w http.ResponseWriter, r *http.Request) {
	p := effectiveModsPath()
	writeJSON(w, 200, map[string]interface{}{
		"version":    appVersion,
		"path":       p,
		"set":        config.ModsPath != "",
		"exists":     p != "" && dirExists(p),
		"candidates": modsCandidates(),
		"instances":  instancesDir(),
	})
}

// /api/path POST {path}
func handleSetPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		writeJSON(w, 400, map[string]string{"error": "Пустой путь"})
		return
	}
	p := strings.TrimSpace(req.Path)
	if err := os.MkdirAll(p, 0o755); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Не удалось создать папку: " + err.Error()})
		return
	}
	config.ModsPath = p
	saveConfig()
	writeJSON(w, 200, map[string]interface{}{"ok": true, "path": p})
}

// /api/installed
func handleInstalled(w http.ResponseWriter, r *http.Request) {
	mods := effectiveModsPath()
	out := []string{}
	if mods != "" {
		if entries, err := os.ReadDir(mods); err == nil {
			for _, e := range entries {
				n := e.Name()
				if !e.IsDir() && (strings.HasSuffix(n, ".jar") || strings.HasSuffix(n, ".jar.disabled")) {
					out = append(out, strings.TrimSuffix(strings.TrimSuffix(n, ".jar"), ".disabled")+".jar")
				}
			}
		}
	}
	writeJSON(w, 200, map[string]interface{}{"mods": out})
}

// /api/open — открыть папку mods
func handleOpen(w http.ResponseWriter, r *http.Request) {
	p := effectiveModsPath()
	if p == "" {
		writeJSON(w, 400, map[string]string{"error": "Папка не задана"})
		return
	}
	_ = os.MkdirAll(p, 0o755)
	openInSystem(p)
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// /api/openurl POST {url}
func handleOpenURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		writeJSON(w, 400, map[string]string{"error": "bad url"})
		return
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "start", "", u.String())
	} else {
		cmd = exec.Command("xdg-open", u.String())
	}
	_ = cmd.Start()
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// /api/tags
func handleTags(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"loaders": []map[string]string{
			{"id": "fabric", "n": "Fabric"}, {"id": "forge", "n": "Forge"},
			{"id": "quilt", "n": "Quilt"}, {"id": "neoforge", "n": "NeoForge"},
		},
		"versions": fetchGameVersions(),
	})
}

// ============================ избранное ============================

func handleFavorites(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var req struct {
			Action string `json:"action"`
			Source string `json:"source"`
			Slug   string `json:"slug"`
			Title  string `json:"title"`
			Icon   string `json:"icon"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Slug == "" {
			writeJSON(w, 400, map[string]string{"error": "bad request"})
			return
		}
		if req.Action == "add" {
			exists := false
			for _, f := range config.Favorites {
				if f.Slug == req.Slug && f.Source == req.Source {
					exists = true
				}
			}
			if !exists {
				config.Favorites = append(config.Favorites, Favorite{Source: req.Source, Slug: req.Slug, Title: req.Title, Icon: req.Icon})
			}
		} else if req.Action == "remove" {
			out := []Favorite{}
			for _, f := range config.Favorites {
				if !(f.Slug == req.Slug && f.Source == req.Source) {
					out = append(out, f)
				}
			}
			config.Favorites = out
		}
		saveConfig()
		writeJSON(w, 200, map[string]interface{}{"ok": true, "count": len(config.Favorites)})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"favorites": config.Favorites})
}

// ============================ обновления модов ============================

// /api/updates — проверка новых версий у скачанных модов
func handleUpdates(w http.ResponseWriter, r *http.Request) {
	m := loadManifest()
	type upd struct {
		Filename  string `json:"filename"`
		Title     string `json:"title"`
		Source    string `json:"source"`
		Current   string `json:"current"`
		Latest    string `json:"latest"`
		URL       string `json:"url"`
		NewFile   string `json:"new_file"`
		VersionID string `json:"version_id"`
		Version   string `json:"version"`
		MC        string `json:"mc"`
		HasUpdate bool   `json:"has_update"`
	}
	out := []upd{}
	// параллельно, но бережно
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for fname, ref := range m.Files {
		wg.Add(1)
		go func(fname string, ref ModRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			u := upd{Filename: fname, Title: ref.Title, Source: ref.Source, Current: ref.Version, Version: ref.Version, MC: ref.MC}
			if ref.Source == "modrinth" {
				body, code := modrinthGet("https://api.modrinth.com/v2/project/" + ref.Project + "/version")
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
					if json.Unmarshal(body, &vers) == nil && len(vers) > 0 {
						latest := vers[0]
						u.Latest = latest.VersionNumber
						u.VersionID = latest.ID
						u.Version = latest.VersionNumber
						u.URL = ""
						for _, f := range latest.Files {
							if f.Primary {
								u.URL = f.URL
							}
						}
						if u.URL == "" && len(latest.Files) > 0 {
							u.URL = latest.Files[0].URL
						}
						u.HasUpdate = latest.ID != ref.VersionID
						for _, f := range latest.Files {
							if f.Primary {
								u.NewFile = f.Filename
							}
						}
						if u.NewFile == "" && len(latest.Files) > 0 {
							u.NewFile = latest.Files[0].Filename
						}
					}
				}
			} else if ref.Source == "cf" {
				body, code := cfwidgetFetch("minecraft/mc-mods/" + ref.Project)
				if code == 200 {
					if proj, err := cfNormalize(body, ref.Project); err == nil && len(proj.Versions) > 0 {
						v := proj.Versions[0]
						for _, vv := range proj.Versions {
							if vv.VersionType == "release" {
								v = vv
								break
							}
						}
						if len(v.Files) > 0 {
							u.URL = v.Files[0].URL
							u.NewFile = v.Files[0].Filename
							u.VersionID = strconv.FormatInt(v.FileID, 10)
						}
						u.Latest = v.VersionNumber
						u.Version = u.Latest
						u.HasUpdate = u.VersionID != ref.VersionID && u.VersionID != ""
					}
				}
			}
			mu.Lock()
			out = append(out, u)
			mu.Unlock()
		}(fname, ref)
	}
	wg.Wait()
	// только актуальные записи (файл ещё существует)
	live := []upd{}
	modsDir := effectiveModsPath()
	for _, u := range out {
		if modsDir == "" {
			break
		}
		if _, err := os.Stat(filepath.Join(modsDir, u.Filename)); err == nil {
			live = append(live, u)
		}
	}
	writeJSON(w, 200, map[string]interface{}{"updates": live})
}

// /api/update POST — скачать новую версию и удалить старую
func handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	if !opMutex.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "Занято, подожди"})
		return
	}
	defer opMutex.Unlock()

	var req struct {
		Filename  string `json:"filename"`
		URL       string `json:"url"`
		NewFile   string `json:"new_file"`
		Source    string `json:"source"`
		Project   string `json:"project"`
		Title     string `json:"title"`
		VersionID string `json:"version_id"`
		Version   string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Filename == "" || req.URL == "" {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	mods := effectiveModsPath()
	if mods == "" {
		writeJSON(w, 400, map[string]string{"error": "no_path"})
		return
	}
	m := loadManifest()
	old, hadRef := m.Files[req.Filename]

	// имя нового файла: из ответа обновлений, из URL или из заголовка
	newName := filepath.Base(req.NewFile)
	if newName == "" || newName == "." || !strings.HasSuffix(newName, ".jar") {
		newName = filepath.Base(req.URL)
	}
	if !strings.HasSuffix(newName, ".jar") {
		// curseforge api/v1 редирект — имени нет, спросим у manifest? возьмём из headers после загрузки
		newName = ""
	}

	progReset("update", req.Title, 1)
	defer progActive.Store(false)

	dest := filepath.Join(mods, newName)
	if newName == "" {
		// скачаем во временный и посмотрим Content-Disposition / финальный URL
		dest = filepath.Join(mods, ".update_tmp")
	}
	if err := downloadToFile(req.URL, dest); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	if newName == "" {
		// у forgecdn имя файла есть в последнем URL; downloadToFile не отдаёт его — используем fallback имя
		final := req.Title + " " + req.Version + ".jar"
		final = strings.Map(func(r rune) rune {
			if strings.ContainsRune(`\/:*?"<>|`, r) {
				return '_'
			}
			return r
		}, final)
		newName = final
		_ = os.Rename(dest, filepath.Join(mods, newName))
	}
	// удаляем старый файл
	if oldPath := filepath.Join(mods, req.Filename); oldPath != filepath.Join(mods, newName) {
		_ = os.Remove(oldPath)
	}
	// обновляем манифест (сохраняем mc из старой записи)
	newRef := ModRef{
		Source: req.Source, Project: req.Project, Title: req.Title,
		Version: req.Version, VersionID: req.VersionID, MC: old.MC,
	}
	_ = hadRef
	delete(m.Files, req.Filename)
	m.Files[newName] = newRef
	saveManifest(m)
	writeJSON(w, 200, map[string]interface{}{"ok": true, "new_file": newName})
}

// ============================ установка сборок ============================

type packInstallReq struct {
	URL             string `json:"url"`
	Filename        string `json:"filename"`
	Name            string `json:"name"`
	Dest            string `json:"dest"` // instance | minecraft | file
	IncludeOptional bool   `json:"include_optional"`
}

func copyZipEntry(zf *zip.File, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(f, rc)
	return err
}

// /api/modpack/install POST
func handleModpackInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	if !opMutex.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "Уже что-то устанавливается"})
		return
	}
	defer opMutex.Unlock()

	var req packInstallReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}

	name := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(req.Name))
	if name == "" {
		name = "modpack"
	}

	progReset("pack-download", "Скачиваю сборку…", 1)
	defer progActive.Store(false)

	tmpMrpack := filepath.Join(os.TempDir(), "craftloader_"+strconv.FormatInt(time.Now().UnixNano(), 10)+".zip")
	if err := downloadToFile(req.URL, tmpMrpack); err != nil {
		writeJSON(w, 502, map[string]string{"error": "Не смог скачать сборку: " + err.Error()})
		return
	}
	defer os.Remove(tmpMrpack)

	if req.Dest == "file" {
		_ = os.MkdirAll(filepath.Join(configDir(), "downloads"), 0o755)
		fn := req.Filename
		if fn == "" {
			fn = name + ".zip"
		}
		dest := filepath.Join(configDir(), "downloads", filepath.Base(fn))
		data, _ := os.ReadFile(tmpMrpack)
		_ = os.WriteFile(dest, data, 0o644)
		writeJSON(w, 200, map[string]interface{}{"ok": true, "file": dest})
		return
	}

	zr, err := zip.OpenReader(tmpMrpack)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "Это не zip/mrpack архив"})
		return
	}
	defer zr.Close()

	// определяем формат: modrinth (modrinth.index.json) или curseforge (manifest.json)
	var mrIndex struct {
		Files []struct {
			Path string `json:"path"`
			Env  struct {
				Client string `json:"client"`
			} `json:"env"`
			Downloads []string `json:"downloads"`
		} `json:"files"`
		Dependencies map[string]string `json:"dependencies"`
	}
	var cfManifest struct {
		Files []struct {
			ProjectID int64 `json:"projectID"`
			FileID    int64 `json:"fileID"`
			Required  bool  `json:"required"`
		} `json:"files"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	format := ""
	for _, f := range zr.File {
		if f.Name == "modrinth.index.json" {
			rc, _ := f.Open()
			data, _ := io.ReadAll(rc)
			rc.Close()
			if json.Unmarshal(data, &mrIndex) == nil {
				format = "modrinth"
			}
			break
		}
	}
	if format == "" {
		for _, f := range zr.File {
			if f.Name == "manifest.json" {
				rc, _ := f.Open()
				data, _ := io.ReadAll(rc)
				rc.Close()
				if json.Unmarshal(data, &cfManifest) == nil {
					format = "curseforge"
				}
				break
			}
		}
	}
	if format == "" {
		writeJSON(w, 400, map[string]string{"error": "В архиве нет манифеста сборки (ни .mrpack, ни CurseForge manifest)"})
		return
	}

	// целевая папка
	var target string
	if req.Dest == "instance" {
		target = filepath.Join(instancesDir(), name)
	} else {
		mods := effectiveModsPath()
		if mods == "" {
			writeJSON(w, 400, map[string]string{"error": "no_path"})
			return
		}
		target = filepath.Dir(mods) // корень .minecraft
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		writeJSON(w, 500, map[string]string{"error": "Не могу создать папку: " + err.Error()})
		return
	}

	// 1) overrides (только в режиме отдельной папки)
	if req.Dest == "instance" {
		for _, f := range zr.File {
			if strings.HasPrefix(f.Name, "overrides/") && f.Name != "overrides/" {
				rel := strings.TrimPrefix(f.Name, "overrides/")
				if dest, ok := safeJoin(target, rel); ok && !f.FileInfo().IsDir() {
					_ = copyZipEntry(f, dest)
				}
			}
		}
	}

	// 2) список файлов на скачивание
	type job struct{ url, path string }
	var jobs []job
	skipped := []string{}

	if format == "modrinth" {
		for _, f := range mrIndex.Files {
			env := f.Env.Client
			if env == "unsupported" {
				continue
			}
			if env == "optional" && !req.IncludeOptional {
				skipped = append(skipped, f.Path+" (необязательный)")
				continue
			}
			picked := ""
			for _, u := range f.Downloads {
				if pu, err := url.Parse(u); err == nil && downloadAllowed(pu) {
					picked = u
					break
				}
			}
			if picked == "" {
				skipped = append(skipped, f.Path+" (источник не разрешён)")
				continue
			}
			jobs = append(jobs, job{url: picked, path: f.Path})
		}
	} else {
		for _, f := range cfManifest.Files {
			if !f.Required && !req.IncludeOptional {
				skipped = append(skipped, fmt.Sprintf("CF #%d (необязательный)", f.FileID))
				continue
			}
			jobs = append(jobs, job{
				url:  fmt.Sprintf("https://www.curseforge.com/api/v1/mods/%d/files/%d/download", f.ProjectID, f.FileID),
				path: "", // имя узнаем из редиректа позже — временно по fileID
			})
		}
	}

	// 3) качаем
	progReset("install", name, int64(len(jobs)))
	installed := []string{}
	failed := []string{}

	for i, j := range jobs {
		progItemsDone.Store(int64(i))
		progLabel.Store(j.path)
		destPath := ""
		if j.path != "" {
			var ok bool
			destPath, ok = safeJoin(target, j.path)
			if !ok {
				failed = append(failed, j.path+" (плохой путь)")
				continue
			}
		} else {
			// curseforge: редирект ведёт на forgecdn с именем — получим его
			finalURL, err := resolveRedirect(j.url)
			if err != nil {
				failed = append(failed, j.url+" ("+err.Error()+")")
				continue
			}
			base := ""
			if pu, perr := url.Parse(finalURL); perr == nil {
				base = filepath.Base(pu.Path) // только путь, без ?query
			}
			if base == "" || base == "/" || !strings.HasSuffix(base, ".jar") {
				base = fmt.Sprintf("cf_%d.jar", time.Now().UnixNano()%100000)
			}
			destPath, _ = safeJoin(target, "mods/"+base)
		}
		if err := downloadToFile(j.url, destPath); err != nil {
			failed = append(failed, filepath.Base(destPath)+" ("+err.Error()+")")
			continue
		}
		installed = append(installed, filepath.Base(destPath))
	}
	progItemsDone.Store(int64(len(jobs)))
	progActive.Store(false)

	// 4) метаданные инстанса
	if req.Dest == "instance" {
		meta := map[string]interface{}{
			"name": name, "format": format, "installed": time.Now().Format("2006-01-02 02:15"),
			"files": len(installed), "source_url": req.URL,
		}
		if format == "modrinth" {
			meta["dependencies"] = mrIndex.Dependencies
		}
		data, _ := json.MarshalIndent(meta, "", "  ")
		_ = os.WriteFile(filepath.Join(target, "craftloader-instance.json"), data, 0o644)
	}

	log.Printf("сборка «%s»: установлено %d, пропущено %d, ошибок %d -> %s", name, len(installed), len(skipped), len(failed), target)
	writeJSON(w, 200, map[string]interface{}{
		"ok": true, "dir": target, "installed": installed,
		"skipped": skipped, "failed": failed, "format": format,
	})
}

// resolveRedirect возвращает финальный URL после редиректов (для получения имени файла)
func resolveRedirect(rawurl string) (string, error) {
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // не следуем — смотрим Location
		},
	}
	req, _ := http.NewRequest("GET", rawurl, nil)
	req.Header.Set("User-Agent", browserUA)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}
	return rawurl, nil // нет редиректа — вернём исходный
}

// /api/curated GET / POST install
func handleCurated(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		if !opMutex.TryLock() {
			writeJSON(w, 409, map[string]string{"error": "Занято"})
			return
		}
		defer opMutex.Unlock()
		var req struct {
			ID     string `json:"id"`
			MC     string `json:"mc"`
			Loader string `json:"loader"`
			Dest   string `json:"dest"` // instance | mods
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MC == "" {
			writeJSON(w, 400, map[string]string{"error": "Выбери версию Minecraft"})
			return
		}
		var pack *CuratedPack
		for i := range curatedPacks {
			if curatedPacks[i].ID == req.ID {
				pack = &curatedPacks[i]
			}
		}
		if pack == nil {
			writeJSON(w, 404, map[string]string{"error": "Сборка не найдена"})
			return
		}
		loader := req.Loader
		if loader == "" {
			loader = "fabric"
		}
		var target string
		if req.Dest == "instance" {
			target = filepath.Join(instancesDir(), pack.ID+"-"+req.MC)
		} else {
			mods := effectiveModsPath()
			if mods == "" {
				writeJSON(w, 400, map[string]string{"error": "no_path"})
				return
			}
			target = mods
		}
		_ = os.MkdirAll(target, 0o755)
		// Fabric/Quilt ищут моды в подпапке mods/
		modDir := target
		if req.Dest == "instance" {
			modDir = filepath.Join(target, "mods")
			_ = os.MkdirAll(modDir, 0o755)
		}

		progReset("install", pack.Name, int64(len(pack.Mods)))
		defer progActive.Store(false)
		installed, skipped := []string{}, []string{}
		for i, slug := range pack.Mods {
			progItemsDone.Store(int64(i))
			progLabel.Store(slug)
			v := url.Values{}
			lj, _ := json.Marshal([]string{loader})
			mj, _ := json.Marshal([]string{req.MC})
			v.Set("loaders", string(lj))
			v.Set("game_versions", string(mj))
			body, code := modrinthGet("https://api.modrinth.com/v2/project/" + slug + "/version?" + v.Encode())
			if code != 200 {
				skipped = append(skipped, slug+" (нет версии для MC "+req.MC+" / "+loader+")")
				continue
			}
			var vers []struct {
				VersionNumber string `json:"version_number"`
				Files         []struct {
					URL      string `json:"url"`
					Filename string `json:"filename"`
					Primary  bool   `json:"primary"`
				} `json:"files"`
			}
			if json.Unmarshal(body, &vers) != nil || len(vers) == 0 {
				skipped = append(skipped, slug+" (нет версии)")
				continue
			}
			file := vers[0].Files[0]
			for _, f := range vers[0].Files {
				if f.Primary {
					file = f
				}
			}
			if err := downloadToFile(file.URL, filepath.Join(modDir, file.Filename)); err != nil {
				skipped = append(skipped, slug+" ("+err.Error()+")")
				continue
			}
			installed = append(installed, file.Filename)
		}
		progItemsDone.Store(int64(len(pack.Mods)))
		// метаданные инстанса — чтобы можно было запустить с страницы «Игра»
		if req.Dest == "instance" {
			meta := map[string]interface{}{
				"name": pack.Name, "format": "craftloader-pack", "mc": req.MC, "loader": loader,
				"installed": time.Now().Format("2006-01-02 15:04"), "files": len(installed),
			}
			data, _ := json.MarshalIndent(meta, "", "  ")
			_ = os.WriteFile(filepath.Join(target, "craftloader-instance.json"), data, 0o644)
		}
		writeJSON(w, 200, map[string]interface{}{
			"ok": true, "dir": target, "installed": installed, "skipped": skipped,
		})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"packs": curatedPacks})
}

// ============================ игра / лаунчер ============================

func handleInstances(w http.ResponseWriter, r *http.Request) {
	type inst struct {
		Name   string `json:"name"`
		Loader string `json:"loader"`
		Path   string `json:"path"`
		MC     string `json:"mc,omitempty"`
	}
	versions := []inst{}
	// .minecraft/versions
	if appdata := os.Getenv("APPDATA"); appdata != "" || runtime.GOOS != "windows" {
		mcRoot := ""
		if runtime.GOOS == "windows" {
			mcRoot = filepath.Join(os.Getenv("APPDATA"), ".minecraft")
		} else {
			home, _ := os.UserHomeDir()
			mcRoot = filepath.Join(home, ".minecraft")
		}
		vdir := filepath.Join(mcRoot, "versions")
		if entries, err := os.ReadDir(vdir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				vjson := filepath.Join(vdir, e.Name(), e.Name()+".json")
				loader := "vanilla"
				if data, err := os.ReadFile(vjson); err == nil {
					s := string(data)
					for _, l := range []string{"fabric", "quilt", "neoforge", "forge"} {
						if strings.Contains(strings.ToLower(s), l) {
							loader = l
							break
						}
					}
				}
				versions = append(versions, inst{Name: e.Name(), Loader: loader, Path: filepath.Join(vdir, e.Name())})
			}
		}
	}
	// Prism Launcher
	prism := []inst{}
	if ld := os.Getenv("LOCALAPPDATA"); ld != "" || runtime.GOOS != "windows" {
		pdir := ""
		if runtime.GOOS == "windows" {
			pdir = filepath.Join(os.Getenv("LOCALAPPDATA"), "PrismLauncher", "instances")
		} else {
			home, _ := os.UserHomeDir()
			pdir = filepath.Join(home, ".local", "share", "PrismLauncher", "instances")
		}
		if entries, err := os.ReadDir(pdir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				if data, err := os.ReadFile(filepath.Join(pdir, e.Name(), "instance.cfg")); err == nil {
					for _, line := range strings.Split(string(data), "\n") {
						if strings.HasPrefix(line, "name=") {
							name = strings.TrimPrefix(line, "name=")
						}
					}
				}
				prism = append(prism, inst{Name: name, Path: filepath.Join(pdir, e.Name())})
			}
		}
	}
	// наши сборки
	cl := []inst{}
	if entries, err := os.ReadDir(instancesDir()); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			it := inst{Name: e.Name(), Path: filepath.Join(instancesDir(), e.Name())}
			// v2.2: читаем метаданные сборки для кнопки запуска
			if data, err := os.ReadFile(filepath.Join(it.Path, "craftloader-instance.json")); err == nil {
				var meta struct {
					MC     string `json:"mc"`
					Loader string `json:"loader"`
				}
				if json.Unmarshal(data, &meta) == nil {
					it.MC, it.Loader = meta.MC, meta.Loader
				}
			}
			cl = append(cl, it)
		}
	}
	// ищем официальный лаунчер
	launcher := ""
	var candidates []string
	if pf := os.Getenv("ProgramFiles(x86)"); pf != "" {
		candidates = append(candidates, filepath.Join(pf, "Minecraft Launcher", "MinecraftLauncher.exe"))
	}
	if ld := os.Getenv("LOCALAPPDATA"); ld != "" {
		candidates = append(candidates, filepath.Join(ld, "Programs", "Minecraft Launcher", "MinecraftLauncher.exe"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			launcher = c
			break
		}
	}
	writeJSON(w, 200, map[string]interface{}{
		"versions": versions, "prism": prism, "craftloader": cl,
		"launcher": launcher, "mc_root_hint": ".minecraft",
	})
}

func handleLaunch(w http.ResponseWriter, r *http.Request) {
	// 1) пробуем exe лаунчера
	if ld := os.Getenv("LOCALAPPDATA"); ld != "" || runtime.GOOS != "windows" {
		var candidates []string
		if pf := os.Getenv("ProgramFiles(x86)"); pf != "" {
			candidates = append(candidates, filepath.Join(pf, "Minecraft Launcher", "MinecraftLauncher.exe"))
		}
		if ld != "" {
			candidates = append(candidates, filepath.Join(ld, "Programs", "Minecraft Launcher", "MinecraftLauncher.exe"))
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				if err := exec.Command(c).Start(); err == nil {
					writeJSON(w, 200, map[string]interface{}{"ok": true, "method": "exe", "path": c})
					return
				}
			}
		}
	}
	// 2) протокол minecraft://
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "start", "", "minecraft://")
	} else {
		cmd = exec.Command("xdg-open", "minecraft://")
	}
	if err := cmd.Start(); err == nil {
		writeJSON(w, 200, map[string]interface{}{"ok": true, "method": "protocol"})
		return
	}
	writeJSON(w, 404, map[string]string{"error": "Лаунчер не найден. Установи официальный лаунчер с minecraft.net"})
}

// /api/openfolder POST {path} — открыть разрешённую папку в проводнике
func handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	p, err := filepath.Abs(strings.TrimSpace(req.Path))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad path"})
		return
	}
	// разрешаем только известные корни
	allowed := []string{configDir(), instancesDir()}
	if m := effectiveModsPath(); m != "" {
		allowed = append(allowed, m, filepath.Dir(m))
	}
	if home, err := os.UserHomeDir(); err == nil {
		if runtime.GOOS == "windows" {
			allowed = append(allowed,
				filepath.Join(home, "AppData", "Roaming", ".minecraft"),
				filepath.Join(home, "AppData", "Local", "PrismLauncher", "instances"))
		} else {
			allowed = append(allowed, filepath.Join(home, ".minecraft"))
		}
	}
	ok := false
	for _, root := range allowed {
		if p == root || strings.HasPrefix(p, root+string(os.PathSeparator)) {
			ok = true
			break
		}
	}
	if !ok {
		writeJSON(w, 403, map[string]string{"error": "Эту папку открывать нельзя"})
		return
	}
	_ = os.MkdirAll(p, 0o755)
	openInSystem(p)
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// ============================ запуск сервера ============================
func buildMux() *http.ServeMux {
	sub, _ := fs.Sub(uiFS, "ui")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/search", handleSearch)
	mux.HandleFunc("/api/project", handleProject)
	mux.HandleFunc("/api/versions", handleVersions)
	mux.HandleFunc("/api/cf/project", handleCFProject)
	mux.HandleFunc("/api/download", handleDownload)
	mux.HandleFunc("/api/progress", handleProgress)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/path", handleSetPath)
	mux.HandleFunc("/api/installed", handleInstalled)
	mux.HandleFunc("/api/open", handleOpen)
	mux.HandleFunc("/api/openfolder", handleOpenFolder)
	mux.HandleFunc("/api/openurl", handleOpenURL)
	mux.HandleFunc("/api/tags", handleTags)
	mux.HandleFunc("/api/favorites", handleFavorites)
	mux.HandleFunc("/api/updates", handleUpdates)
	mux.HandleFunc("/api/update", handleUpdate)
	mux.HandleFunc("/api/modpack/install", handleModpackInstall)
	mux.HandleFunc("/api/curated", handleCurated)
	mux.HandleFunc("/api/instances", handleInstances)
	mux.HandleFunc("/api/packs", handlePacks)
	mux.HandleFunc("/api/packs/install", handlePackInstall)
	mux.HandleFunc("/api/launch/info", handleLaunchInfo)
	mux.HandleFunc("/api/launch/run", handleLaunchRun)
	mux.HandleFunc("/api/launch/java", handleLaunchJava)
	mux.HandleFunc("/api/launch", handleLaunch)
	return mux
}

func initLog() {
	_ = os.MkdirAll(configDir(), 0o755)
	f, err := os.OpenFile(filepath.Join(configDir(), "log.txt"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		log.SetOutput(f)
	}
}

func main() {
	initLog()
	loadConfig()
	ensureModsPath()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	addr := "http://" + ln.Addr().String()

	srv := &http.Server{Handler: buildMux()}
	go func() {
		if err := srv.Serve(ln); err != nil {
			log.Fatalf("serve: %v", err)
		}
	}()

	log.Printf("CraftLoader %s слушает %s", appVersion, addr)
	runUI(addr)
}
