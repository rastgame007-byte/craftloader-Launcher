package main

// launcher.go — скачивание файлов игры с серверов Mojang/Fabric/Quilt
// и запуск Minecraft (оффлайн-режим, по нику).

import (
	"archive/zip"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const javaRuntimeAllURL = "https://piston-meta.mojang.com/v1/products/java-runtime/2ec0cc96c44e5a76b9c8b7c39df7210883d12871/all.json"

func runtimeDir() string   { return filepath.Join(configDir(), "runtime") }
func assetsDir() string    { return filepath.Join(runtimeDir(), "assets") }
func librariesDir() string { return filepath.Join(runtimeDir(), "libraries") }
func versionsDir() string  { return filepath.Join(runtimeDir(), "versions") }

func mcOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "osx"
	default:
		return "linux"
	}
}

func javaBinName() string {
	if runtime.GOOS == "windows" {
		return "javaw.exe"
	}
	return "java"
}

// ============================ правила ============================

type mcRule struct {
	Action string `json:"action"`
	Os     *struct {
		Name string `json:"name"`
		Arch string `json:"arch"`
	} `json:"os"`
	Features map[string]interface{} `json:"features"`
}

func evalRules(rules []mcRule) bool {
	if len(rules) == 0 {
		return true
	}
	allow := false
	for _, r := range rules {
		matched := true
		if r.Os != nil {
			if r.Os.Name != "" && r.Os.Name != mcOS() {
				matched = false
			}
			if r.Os.Arch != "" {
				if r.Os.Arch == "x86" { // 32-бит — не мы
					matched = false
				}
			}
		}
		if len(r.Features) > 0 { // фичи (demo, кастомное разрешение) не включаем
			matched = false
		}
		if matched {
			allow = r.Action == "allow"
		}
	}
	return allow
}

// ============================ структуры версии ============================

type mcArtifact struct {
	Path string `json:"path"`
	URL  string `json:"url"`
	Sha1 string `json:"sha1"`
	Size int64  `json:"size"`
}

type mcLibrary struct {
	Name      string            `json:"name"`
	Rules     []mcRule          `json:"rules"`
	Natives   map[string]string `json:"natives"`
	URL       string            `json:"url"` // fabric-формат: maven-репозиторий
	Downloads struct {
		Artifact    *mcArtifact            `json:"artifact"`
		Classifiers map[string]*mcArtifact `json:"classifiers"`
	} `json:"downloads"`
}

type mcVersion struct {
	ID                 string `json:"id"`
	InheritsFrom       string `json:"inheritsFrom"`
	MainClass          string `json:"mainClass"`
	MinecraftArguments string `json:"minecraftArguments"`
	Arguments          *struct {
		Game []interface{} `json:"game"`
		JVM  []interface{} `json:"jvm"`
	} `json:"arguments"`
	AssetIndex struct {
		ID        string `json:"id"`
		URL       string `json:"url"`
		Size      int64  `json:"size"`
		TotalSize int64  `json:"totalSize"`
	} `json:"assetIndex"`
	Assets    string `json:"assets"`
	Downloads struct {
		Client *mcArtifact `json:"client"`
	} `json:"downloads"`
	Libraries []mcLibrary `json:"libraries"`
	Logging   struct {
		Client struct {
			Argument string `json:"argument"`
			File     struct {
				ID   string `json:"id"`
				URL  string `json:"url"`
				Sha1 string `json:"sha1"`
				Size int64  `json:"size"`
			} `json:"file"`
		} `json:"client"`
	} `json:"logging"`
	Type        string `json:"type"`
	JavaVersion struct {
		Component    string `json:"component"`
		MajorVersion int    `json:"majorVersion"`
	} `json:"javaVersion"`
	ReleaseTime string `json:"releaseTime"`
}

// mavenPath: "group:artifact:version[:classifier]" -> путь в репозитории
func mavenPath(name, classifier string) string {
	parts := strings.Split(name, ":")
	if len(parts) < 3 {
		return strings.ReplaceAll(name, ":", "_") + ".jar"
	}
	group := strings.ReplaceAll(parts[0], ".", "/")
	file := parts[1] + "-" + parts[2]
	if classifier != "" {
		file += "-" + classifier
	}
	return group + "/" + parts[1] + "/" + parts[2] + "/" + file + ".jar"
}

// ============================ кэш манифестов ============================

var metaCache = struct {
	mu   sync.Mutex
	data map[string][]byte
	at   map[string]time.Time
}{data: map[string][]byte{}, at: map[string]time.Time{}}

func metaGet(u string) ([]byte, int) {
	metaCache.mu.Lock()
	if b, ok := metaCache.data[u]; ok && time.Since(metaCache.at[u]) < 10*time.Minute {
		metaCache.mu.Unlock()
		return b, 200
	}
	metaCache.mu.Unlock()
	b, code := httpGet(u, userAgent)
	if code == 200 {
		metaCache.mu.Lock()
		metaCache.data[u] = b
		metaCache.at[u] = time.Now()
		metaCache.mu.Unlock()
	}
	return b, code
}

type mcManifest struct {
	Latest struct {
		Release  string `json:"release"`
		Snapshot string `json:"snapshot"`
	} `json:"latest"`
	Versions []struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		URL         string `json:"url"`
		ReleaseTime string `json:"releaseTime"`
	} `json:"versions"`
}

func fetchManifest() (*mcManifest, error) {
	b, code := metaGet("https://piston-meta.mojang.com/mc/game/version_manifest_v2.json")
	if code != 200 {
		return nil, fmt.Errorf("не удалось получить список версий Mojang")
	}
	var m mcManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ============================ разрешение версии ============================

type loaderInfo struct {
	Fabric []string `json:"fabric"`
	Quilt  []string `json:"quilt"`
}

func latestFabricLoader() (string, error) {
	b, code := metaGet("https://meta.fabricmc.net/v2/versions/loader")
	if code != 200 {
		return "", fmt.Errorf("meta.fabricmc.net недоступен")
	}
	var list []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if json.Unmarshal(b, &list) != nil || len(list) == 0 {
		return "", fmt.Errorf("fabric: пустой ответ")
	}
	for _, l := range list {
		if l.Stable {
			return l.Version, nil
		}
	}
	return list[0].Version, nil
}

func latestQuiltLoader() (string, error) {
	b, code := metaGet("https://meta.quiltmc.org/v3/versions/loader")
	if code != 200 {
		return "", fmt.Errorf("meta.quiltmc.org недоступен")
	}
	var list []struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &list) != nil || len(list) == 0 {
		return "", fmt.Errorf("quilt: пустой ответ")
	}
	for _, l := range list {
		if !strings.Contains(l.Version, "beta") && !strings.Contains(l.Version, "pre") {
			return l.Version, nil
		}
	}
	return list[0].Version, nil
}

// resolveVersion: скачивает JSON версии, накладывает fabric/quilt при необходимости
func resolveVersion(version, loader, loaderVersion string) (*mcVersion, error) {
	man, err := fetchManifest()
	if err != nil {
		return nil, err
	}
	var vurl string
	for _, v := range man.Versions {
		if v.ID == version {
			vurl = v.URL
			break
		}
	}
	if vurl == "" {
		return nil, fmt.Errorf("версия %s не найдена", version)
	}
	b, code := metaGet(vurl)
	if code != 200 {
		return nil, fmt.Errorf("не удалось скачать json версии %s", version)
	}
	var base mcVersion
	if err := json.Unmarshal(b, &base); err != nil {
		return nil, err
	}
	if loader == "" || loader == "vanilla" {
		return &base, nil
	}

	var profileURL string
	switch loader {
	case "fabric":
		if loaderVersion == "" {
			if loaderVersion, err = latestFabricLoader(); err != nil {
				return nil, err
			}
		}
		profileURL = "https://meta.fabricmc.net/v2/versions/loader/" + version + "/" + loaderVersion + "/profile/json"
	case "quilt":
		if loaderVersion == "" {
			if loaderVersion, err = latestQuiltLoader(); err != nil {
				return nil, err
			}
		}
		profileURL = "https://meta.quiltmc.org/v3/versions/loader/" + version + "/" + loaderVersion + "/profile/json"
	default:
		return nil, fmt.Errorf("запуск %s не поддерживается (используй Prism) — поддержаны vanilla, fabric, quilt", loader)
	}
	pb, pcode := metaGet(profileURL)
	if pcode != 200 {
		return nil, fmt.Errorf("не удалось получить профиль %s %s", loader, loaderVersion)
	}
	var ov mcVersion
	if err := json.Unmarshal(pb, &ov); err != nil {
		return nil, err
	}
	// мержим overlay в базу
	merged := base
	merged.Libraries = append(append([]mcLibrary{}, ov.Libraries...), base.Libraries...)
	if ov.MainClass != "" {
		merged.MainClass = ov.MainClass
	}
	if ov.ID != "" {
		merged.ID = ov.ID
	}
	if ov.Arguments != nil {
		game := []interface{}{}
		jvm := []interface{}{}
		if base.Arguments != nil {
			game = append(game, base.Arguments.Game...)
			jvm = append(jvm, base.Arguments.JVM...)
		} else if base.MinecraftArguments != "" {
			for _, s := range strings.Fields(base.MinecraftArguments) {
				game = append(game, s)
			}
			base.MinecraftArguments = ""
		}
		game = append(game, ov.Arguments.Game...)
		jvm = append(jvm, ov.Arguments.JVM...)
		merged.Arguments = &struct {
			Game []interface{} `json:"game"`
			JVM  []interface{} `json:"jvm"`
		}{Game: game, JVM: jvm}
	}
	return &merged, nil
}

// ============================ загрузка файлов ============================

type dlJob struct {
	url, dest string
	size      int64
	essential bool
}

func fileOK(path string, size int64) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if size > 0 && st.Size() != size {
		return false
	}
	return true
}

func runJobs(jobs []dlJob) error {
	need := []dlJob{}
	for _, j := range jobs {
		if !fileOK(j.dest, j.size) {
			need = append(need, j)
		}
	}
	progItemsTotal := int64(len(need))
	progItemsDone.Store(0)
	progReset("launch-download", "Скачиваю файлы игры", progItemsTotal)
	if len(need) == 0 {
		return nil
	}
	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 8)
		errMu   sync.Mutex
		errs    []string
		doneCnt int64
	)
	for _, j := range need {
		wg.Add(1)
		go func(j dlJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := downloadToFileQuiet(j.url, j.dest); err != nil {
				errMu.Lock()
				errs = append(errs, filepath.Base(j.dest)+": "+err.Error())
				errMu.Unlock()
				return
			}
			n := progItemsDone.Add(1)
			progLabel.Store(j.dest)
			_ = n
			_ = doneCnt
		}(j)
	}
	wg.Wait()
	essential := 0
	for _, e := range errs {
		if strings.Contains(e, "essential") {
			essential++
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		msg := "Не скачались файлы (" + fmt.Sprint(len(errs)) + "): " + strings.Join(errs[:min(3, len(errs))], "; ")
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// downloadToFileQuiet — как downloadToFile, но не трогает байтовый прогресс
func downloadToFileQuiet(rawurl, dest string) error {
	u, err := url.Parse(rawurl)
	if err != nil || !downloadAllowed(u) {
		return fmt.Errorf("источник не разрешён")
	}
	req, _ := http.NewRequest("GET", u.String(), nil)
	req.Header.Set("User-Agent", browserUA)
	resp, err := dlClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func extractNatives(jarPath, destDir string) {
	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return
	}
	defer zr.Close()
	_ = os.MkdirAll(destDir, 0o755)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || strings.HasPrefix(f.Name, "META-INF") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		out := filepath.Join(destDir, filepath.FromSlash(f.Name))
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		of, err := os.Create(out)
		if err != nil {
			rc.Close()
			continue
		}
		_, _ = io.Copy(of, rc)
		of.Close()
		rc.Close()
	}
}

// ============================ Java ============================

type javaCand struct {
	path string
	ver  string
}

func javaCandidates() []javaCand {
	var out []javaCand
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		if abs, err := filepath.Abs(p); err == nil {
			if seen[abs] {
				return
			}
			seen[abs] = true
		}
		out = append(out, javaCand{path: p, ver: javaVersionOf(p)})
	}
	// 1) наш скачанный рантайм Mojang
	if m, _ := filepath.Glob(filepath.Join(runtimeDir(), "java", "*", "bin", javaBinName())); len(m) > 0 {
		add(m[len(m)-1])
	}
	// 2) PATH
	if p, err := exec.LookPath(javaBinName()); err == nil {
		add(p)
	}
	if p, err := exec.LookPath("java"); err == nil {
		add(p)
	}
	// 3) JAVA_HOME
	if jh := os.Getenv("JAVA_HOME"); jh != "" {
		add(filepath.Join(jh, "bin", javaBinName()))
	}
	// 4) типичные места
	var globs []string
	if runtime.GOOS == "windows" {
		pf := os.Getenv("ProgramFiles")
		globs = []string{
			filepath.Join(pf, "Java", "*", "bin", javaBinName()),
			filepath.Join(pf, "Eclipse Adoptium", "*", "bin", javaBinName()),
			filepath.Join(pf, "Microsoft", "jdk-*", "bin", javaBinName()),
			filepath.Join(pf, "Zulu", "*", "bin", javaBinName()),
			filepath.Join(pf, "Amazon Corretto", "*", "bin", javaBinName()),
			filepath.Join(pf, "BellSoft", "*", "bin", javaBinName()),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "Java", "*", "bin", javaBinName()),
		}
	} else {
		globs = []string{"/usr/lib/jvm/*/bin/java", "/opt/java/*/bin/java"}
	}
	for _, g := range globs {
		if m, _ := filepath.Glob(g); len(m) > 0 {
			for _, p := range m {
				add(p)
			}
		}
	}
	return out
}

// javaMajor: "1.8.0_392"->8, "11.0.2"->11, "17.0.9"->17
func javaMajor(ver string) int {
	v := strings.TrimPrefix(ver, "1.")
	f := strings.Split(v, ".")
	n := 0
	if len(f) > 0 {
		fmt.Sscanf(f[0], "%d", &n)
	}
	return n
}

// findJavaFor: ищет Java >= required (0 — любая), предпочитаем наименьшую подходящую
func findJavaFor(required int) (string, string) {
	cands := javaCandidates()
	bestPath, bestVer := "", ""
	bestMaj := 999
	for _, c := range cands {
		maj := javaMajor(c.ver)
		if maj == 0 {
			continue // не смогли понять версию — пропускаем
		}
		if required > 0 && maj < required {
			continue
		}
		if maj < bestMaj {
			bestPath, bestVer, bestMaj = c.path, c.ver, maj
		}
	}
	if bestPath != "" {
		return bestPath, bestVer
	}
	// ничего подходящего: вернуть первую попавшуюся (для инфо)
	if len(cands) > 0 {
		return cands[0].path, cands[0].ver
	}
	return "", ""
}

func findJava() (string, string) {
	// самый свежий из найденных — его покажу в статусе
	cands := javaCandidates()
	bestPath, bestVer := "", ""
	bestMaj := -1
	for _, c := range cands {
		if maj := javaMajor(c.ver); maj > bestMaj {
			bestPath, bestVer, bestMaj = c.path, c.ver, maj
		}
	}
	return bestPath, bestVer
}

func javaVersionOf(path string) string {
	cmd := exec.Command(path, "-version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "?"
	}
	s := string(out)
	if i := strings.Index(s, "version \""); i >= 0 {
		rest := s[i+9:]
		if j := strings.Index(rest, "\""); j > 0 {
			return rest[:j]
		}
	}
	return "?"
}

// downloadJavaRuntime качает JRE от Mojang в runtime/java/<component>
func downloadJavaRuntime(wantComponent string) (string, error) {
	b, code := metaGet(javaRuntimeAllURL)
	if code != 200 {
		return "", fmt.Errorf("манифест Java-рантаймов недоступен")
	}
	var all map[string]map[string][]struct {
		Manifest struct {
			URL string `json:"url"`
		} `json:"manifest"`
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
	}
	if err := json.Unmarshal(b, &all); err != nil {
		return "", err
	}
	platform := ""
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		platform = "windows-x64"
	case "windows/386":
		platform = "windows-x86"
	case "linux/amd64":
		platform = "linux"
	case "darwin/amd64":
		platform = "mac-os-x64"
	case "darwin/arm64":
		platform = "mac-arm64"
	}
	if platform == "" {
		return "", fmt.Errorf("платформа не поддерживается")
	}
	byComp, ok := all[platform]
	if !ok {
		return "", fmt.Errorf("Java-рантайм для %s недоступен", platform)
	}
	component := ""
	if wantComponent != "" && len(byComp[wantComponent]) > 0 {
		component = wantComponent
	}
	if component == "" {
		for _, pref := range []string{"java-runtime-delta", "java-runtime-gamma", "java-runtime-alpha", "jre-legacy"} {
			if len(byComp[pref]) > 0 {
				component = pref
				break
			}
		}
	}
	if component == "" {
		for k := range byComp {
			component = k
			break
		}
	}
	entries := byComp[component]
	if len(entries) == 0 {
		return "", fmt.Errorf("пустой список рантаймов")
	}
	mb, mcode := metaGet(entries[0].Manifest.URL)
	if mcode != 200 {
		return "", fmt.Errorf("манифест файлов Java недоступен")
	}
	var mf struct {
		Files map[string]struct {
			Type       string `json:"type"`
			Executable bool   `json:"executable"`
			Downloads  struct {
				Raw struct {
					URL  string `json:"url"`
					Sha1 string `json:"sha1"`
					Size int64  `json:"size"`
				} `json:"raw"`
			} `json:"downloads"`
		} `json:"files"`
	}
	if err := json.Unmarshal(mb, &mf); err != nil {
		return "", err
	}
	root := filepath.Join(runtimeDir(), "java", component)
	jobs := []dlJob{}
	execFiles := []string{}
	for path, f := range mf.Files {
		if f.Type != "file" || f.Downloads.Raw.URL == "" {
			continue
		}
		dest := filepath.Join(root, filepath.FromSlash(path))
		if f.Executable {
			execFiles = append(execFiles, dest)
		}
		jobs = append(jobs, dlJob{url: f.Downloads.Raw.URL, dest: dest, size: f.Downloads.Raw.Size})
	}
	if len(jobs) == 0 {
		return "", fmt.Errorf("в манифесте нет файлов")
	}
	if err := runJobs(jobs); err != nil {
		return "", err
	}
	for _, p := range execFiles {
		_ = os.Chmod(p, 0o755)
	}
	return root, nil
}

// ============================ оффлайн-UUID ============================

func offlineUUID(name string) string {
	h := md5.Sum([]byte("OfflinePlayer:" + name))
	b := h[:]
	b[6] = (b[6] & 0x0F) | 0x30 // версия 3
	b[8] = (b[8] & 0x3F) | 0x80 // вариант
	hexs := hex.EncodeToString(b)
	return hexs[0:8] + "-" + hexs[8:12] + "-" + hexs[12:16] + "-" + hexs[16:20] + "-" + hexs[20:32]
}

// ============================ сборка и запуск ============================

type launchReq struct {
	Version       string `json:"version"`
	Loader        string `json:"loader"`
	LoaderVersion string `json:"loader_version"`
	Nickname      string `json:"nickname"`
	GameDir       string `json:"game_dir"`
	Ram           int    `json:"ram"` // ГБ
}

func flattenArgs(list []interface{}, vars map[string]string) []string {
	out := []string{}
	for _, it := range list {
		switch v := it.(type) {
		case string:
			out = append(out, substVars(v, vars))
		case map[string]interface{}:
			b, _ := json.Marshal(v)
			var av struct {
				Value interface{} `json:"value"`
				Rules []mcRule    `json:"rules"`
			}
			if json.Unmarshal(b, &av) != nil {
				continue
			}
			if !evalRules(av.Rules) {
				continue
			}
			switch vv := av.Value.(type) {
			case string:
				out = append(out, substVars(vv, vars))
			case []interface{}:
				for _, s := range vv {
					if ss, ok := s.(string); ok {
						out = append(out, substVars(ss, vars))
					}
				}
			}
		}
	}
	return out
}

func substVars(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return s
}

func doLaunch(req launchReq) (map[string]interface{}, error) {
	if req.Version == "" {
		return nil, fmt.Errorf("не выбрана версия")
	}
	if req.Nickname == "" {
		req.Nickname = "Steve"
	}
	if req.Ram <= 0 {
		req.Ram = 2
	}
	loader := req.Loader
	if loader == "" {
		loader = "vanilla"
	}

	v, err := resolveVersion(req.Version, loader, req.LoaderVersion)
	if err != nil {
		return nil, err
	}
	vid := v.ID

	gameDir := filepath.Join(runtimeDir(), "game")
	if strings.HasPrefix(req.GameDir, "instance:") {
		gameDir = filepath.Join(instancesDir(), strings.TrimPrefix(req.GameDir, "instance:"))
	} else if req.GameDir != "" {
		gameDir = req.GameDir
	}
	if err := os.MkdirAll(gameDir, 0o755); err != nil {
		return nil, fmt.Errorf("не могу создать папку игры: " + err.Error())
	}

	// ----- собираем список загрузок -----
	jobs := []dlJob{}
	classpath := []string{}
	nativesDir := filepath.Join(versionsDir(), vid, "natives-"+mcOS())

	// клиент
	if v.Downloads.Client != nil && v.Downloads.Client.URL != "" {
		jar := filepath.Join(versionsDir(), vid, vid+".jar")
		jobs = append(jobs, dlJob{url: v.Downloads.Client.URL, dest: jar, size: v.Downloads.Client.Size})
	}

	// библиотеки
	nativeJars := []string{}
	for _, lib := range v.Libraries {
		if !evalRules(lib.Rules) {
			continue
		}
		if len(lib.Natives) > 0 {
			cls := lib.Natives[mcOS()]
			if cls == "" {
				continue
			}
			var art *mcArtifact
			if lib.Downloads.Classifiers != nil && lib.Downloads.Classifiers[cls] != nil {
				art = lib.Downloads.Classifiers[cls]
			} else if lib.URL != "" {
				p := mavenPath(lib.Name, cls)
				art = &mcArtifact{Path: p, URL: strings.TrimSuffix(lib.URL, "/") + "/" + p}
			}
			if art != nil {
				dest := filepath.Join(librariesDir(), filepath.FromSlash(art.Path))
				jobs = append(jobs, dlJob{url: art.URL, dest: dest, size: art.Size})
				nativeJars = append(nativeJars, dest)
			}
			continue
		}
		var art *mcArtifact
		if lib.Downloads.Artifact != nil && lib.Downloads.Artifact.URL != "" {
			art = lib.Downloads.Artifact
		} else if lib.URL != "" {
			p := mavenPath(lib.Name, "")
			art = &mcArtifact{Path: p, URL: strings.TrimSuffix(lib.URL, "/") + "/" + p}
		}
		if art == nil || art.URL == "" {
			continue
		}
		dest := filepath.Join(librariesDir(), filepath.FromSlash(art.Path))
		jobs = append(jobs, dlJob{url: art.URL, dest: dest, size: art.Size})
		classpath = append(classpath, dest)
	}
	// клиент в classpath тоже
	if v.Downloads.Client != nil && v.Downloads.Client.URL != "" {
		classpath = append(classpath, filepath.Join(versionsDir(), vid, vid+".jar"))
	}

	// лог-конфиг
	logCfg := ""
	if v.Logging.Client.File.URL != "" {
		logCfg = filepath.Join(assetsDir(), "log_configs", v.Logging.Client.File.ID)
		jobs = append(jobs, dlJob{url: v.Logging.Client.File.URL, dest: logCfg, size: v.Logging.Client.File.Size})
	}

	// ассеты
	idxID := v.AssetIndex.ID
	if idxID == "" {
		idxID = v.Assets
	}
	if idxID != "" && v.AssetIndex.URL != "" {
		idxPath := filepath.Join(assetsDir(), "indexes", idxID+".json")
		jobs = append(jobs, dlJob{url: v.AssetIndex.URL, dest: idxPath, size: v.AssetIndex.Size})
	}

	// прогреваем кэш индекса и добавляем объекты ассетов
	if v.AssetIndex.URL != "" {
		b, code := metaGet(v.AssetIndex.URL)
		if code == 200 {
			var idx struct {
				Objects map[string]struct {
					Hash string `json:"hash"`
					Size int64  `json:"size"`
				} `json:"objects"`
			}
			if json.Unmarshal(b, &idx) == nil {
				for _, o := range idx.Objects {
					if len(o.Hash) < 2 {
						continue
					}
					p := filepath.Join(assetsDir(), "objects", o.Hash[:2], o.Hash)
					u := "https://resources.download.minecraft.net/" + o.Hash[:2] + "/" + o.Hash
					jobs = append(jobs, dlJob{url: u, dest: p, size: o.Size})
				}
			}
		}
	}

	// ----- качаем -----
	if err := runJobs(jobs); err != nil {
		return nil, err
	}
	// распаковываем нативные библиотеки
	for _, nj := range nativeJars {
		extractNatives(nj, nativesDir)
	}

	// ----- Java (по требованию версии, при необходимости скачаем рантайм Mojang) -----
	required := v.JavaVersion.MajorVersion
	if required == 0 {
		required = 8
	}
	javaPath, javaVer := findJavaFor(required)
	if javaMajor(javaVer) < required {
		progPhase.Store("launch-java")
		progLabel.Store("Скачиваю Java-рантайм Mojang…")
		if _, err := downloadJavaRuntime(v.JavaVersion.Component); err != nil {
			log.Printf("java download: %v", err)
			return nil, fmt.Errorf("java_not_found")
		}
		javaPath, javaVer = findJavaFor(required)
		if javaPath == "" || javaMajor(javaVer) < required {
			return nil, fmt.Errorf("java_not_found")
		}
	}

	// ----- переменные -----
	vars := map[string]string{
		"auth_player_name":    req.Nickname,
		"auth_uuid":           offlineUUID(req.Nickname),
		"auth_access_token":   "0",
		"auth_type":           "legacy",
		"user_type":           "legacy",
		"auth_xuid":           "0",
		"clientid":            "0",
		"user_properties":     "{}",
		"version_name":        vid,
		"version_type":        v.Type,
		"game_directory":      gameDir,
		"assets_root":         assetsDir(),
		"assets_index_name":   idxID,
		"launcher_name":       "CraftLoader",
		"launcher_version":    appVersion,
		"classpath":           strings.Join(classpath, string(os.PathListSeparator)),
		"classpath_separator": string(os.PathListSeparator),
		"natives_directory":   nativesDir,
		"library_directory":   librariesDir(),
		"path":                logCfg,
	}

	// ----- аргументы -----
	jvmArgs := []string{"-Xmx" + fmt.Sprint(req.Ram) + "G", "-Dfile.encoding=UTF-8"}
	if v.Arguments != nil && len(v.Arguments.JVM) > 0 {
		jvmArgs = append(jvmArgs, flattenArgs(v.Arguments.JVM, vars)...)
	} else {
		jvmArgs = append(jvmArgs, "-Djava.library.path="+nativesDir, "-cp", vars["classpath"])
	}
	var gameArgs []string
	if v.Arguments != nil && len(v.Arguments.Game) > 0 {
		gameArgs = flattenArgs(v.Arguments.Game, vars)
	} else if v.MinecraftArguments != "" {
		for _, s := range strings.Fields(v.MinecraftArguments) {
			gameArgs = append(gameArgs, substVars(s, vars))
		}
	}
	if v.MainClass == "" {
		return nil, fmt.Errorf("в json версии нет mainClass")
	}

	full := append(append(jvmArgs, v.MainClass), gameArgs...)

	// dry-run для тестов
	if os.Getenv("CL_DRYRUN") != "" {
		log.Printf("[DRYRUN] java: %s", javaPath)
		log.Printf("[DRYRUN] args: %s", strings.Join(full, " "))
		return map[string]interface{}{"ok": true, "dry": true, "java": javaPath, "game_dir": gameDir, "cmd": strings.Join(full, " ")[:200]}, nil
	}

	progPhase.Store("launch")
	progLabel.Store("Запускаю Minecraft…")

	cmd := exec.Command(javaPath, full...)
	cmd.Dir = gameDir
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8", "LANG=C.UTF-8")
	hideWindow(cmd)
	// вывод игры пишем в файл — помогает при проблемах
	if lf, err := os.Create(filepath.Join(gameDir, "craftloader-launch.log")); err == nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("не удалось запустить: " + err.Error())
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()

	log.Printf("запущен Minecraft %s (java %s) pid=%d, gameDir=%s", vid, javaVer, pid, gameDir)
	return map[string]interface{}{
		"ok": true, "java": javaVer, "java_path": javaPath,
		"game_dir": gameDir, "pid": pid, "version": vid,
	}, nil
}

// ============================ обработчики ============================

func handleLaunchInfo(w http.ResponseWriter, r *http.Request) {
	man, err := fetchManifest()
	versions := []map[string]string{}
	if err == nil {
		n := 0
		for _, v := range man.Versions {
			if v.Type != "release" {
				continue
			}
			versions = append(versions, map[string]string{"id": v.ID, "release_time": v.ReleaseTime})
			n++
			if n >= 120 {
				break
			}
		}
	}
	fabric := []string{}
	if b, code := metaGet("https://meta.fabricmc.net/v2/versions/loader"); code == 200 {
		var list []struct {
			Version string `json:"version"`
			Stable  bool   `json:"stable"`
		}
		if json.Unmarshal(b, &list) == nil {
			for _, l := range list {
				if l.Stable {
					fabric = append(fabric, l.Version)
					if len(fabric) >= 10 {
						break
					}
				}
			}
		}
	}
	quilt := []string{}
	if b, code := metaGet("https://meta.quiltmc.org/v3/versions/loader"); code == 200 {
		var list []struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(b, &list) == nil {
			for _, l := range list {
				if !strings.Contains(l.Version, "beta") && !strings.Contains(l.Version, "pre") {
					quilt = append(quilt, l.Version)
					if len(quilt) >= 10 {
						break
					}
				}
			}
		}
	}
	jp, jv := findJava()
	writeJSON(w, 200, map[string]interface{}{
		"versions": versions,
		"fabric":   fabric,
		"quilt":    quilt,
		"java":     map[string]interface{}{"found": jp != "", "path": jp, "version": jv},
		"nickname": config.Nickname,
	})
}

func handleLaunchRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	if !opMutex.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "Уже что-то качается/запускается"})
		return
	}
	defer opMutex.Unlock()

	var req launchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if req.Nickname != "" {
		config.Nickname = req.Nickname
		saveConfig()
	}
	progActive.Store(true)
	defer progActive.Store(false)
	res, err := doLaunch(req)
	if err != nil {
		if err.Error() == "java_not_found" {
			writeJSON(w, 400, map[string]string{"error": "java_not_found"})
			return
		}
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, res)
}

func handleLaunchJava(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return
	}
	if !opMutex.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "Занято"})
		return
	}
	defer opMutex.Unlock()
	progActive.Store(true)
	defer progActive.Store(false)
	root, err := downloadJavaRuntime("")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "path": root})
}
