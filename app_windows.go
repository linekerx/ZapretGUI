package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed ui/index.html
var indexHTML string

//go:embed app.ico
var iconData []byte

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	dwmapi               = windows.NewLazySystemDLL("dwmapi.dll")
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	pShowWindow          = user32.NewProc("ShowWindow")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pIsWindowVisible     = user32.NewProc("IsWindowVisible")
	pSetWindowLongPtrW   = user32.NewProc("SetWindowLongPtrW")
	pCallWindowProcW     = user32.NewProc("CallWindowProcW")
	pMessageBoxW         = user32.NewProc("MessageBoxW")
	pDwmSetWindowAttr    = dwmapi.NewProc("DwmSetWindowAttribute")
)

const (
	swHide, swShow, swRestore = 0, 5, 9
	wmClose                   = 0x0010
	gwlpWndProc               = ^uintptr(3) // -4
)

type App struct {
	w        webview2.WebView
	hwnd     uintptr
	mu       sync.Mutex
	st       Settings
	strats   []Strategy
	run      Runner
	tray     *Tray
	quitting atomic.Bool
	scanning atomic.Bool
	cancel   atomic.Bool
	oldProc  uintptr
	lastLogs []string
}

func msgBox(text string) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(AppName)
	pMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x40)
}

func main() {
	name, _ := windows.UTF16PtrFromString("Global\\ZapretGUI_SingleInstance")
	if _, err := windows.CreateMutex(nil, false, name); err == windows.ERROR_ALREADY_EXISTS {
		msgBox("ZapretGUI уже запущен — ищите значок в трее.")
		return
	}
	if err := os.MkdirAll(DataDir(), 0755); err != nil {
		msgBox("Не удалось создать папку " + DataDir() + ":\n" + err.Error())
		return
	}
	a := &App{st: LoadSettings()}
	extractErr := ExtractAssets()
	_ = ApplyIPSetMode(a.st.IPSetMode)
	var err error
	if a.strats, err = LoadStrategies(); err != nil || len(a.strats) == 0 {
		msgBox("Не найдены стратегии")
		return
	}
	if a.find(a.st.Strategy) < 0 {
		a.st.Strategy = a.strats[0].Name
	}

	a.w = webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  filepath.Join(DataDir(), "webview"),
		WindowOptions: webview2.WindowOptions{
			Title: "ZapretGUI", Width: 1100, Height: 720, IconId: 2, Center: true,
		},
	})
	if a.w == nil {
		msgBox("Не удалось запустить интерфейс: нужен Microsoft Edge WebView2 Runtime.\n" +
			"Скачайте его: https://go.microsoft.com/fwlink/p/?LinkId=2124703")
		OpenURL("https://go.microsoft.com/fwlink/p/?LinkId=2124703")
		return
	}
	defer a.w.Destroy()
	a.hwnd = uintptr(a.w.Window())
	a.w.SetSize(900, 600, webview2.HintMin)
	a.styleWindow()
	a.hookClose()

	a.run.Log = func(s string) { a.log(s) }
	a.run.Exit = func(err error) {
		if a.scanning.Load() {
			return
		}
		if err != nil && !a.quitting.Load() {
			a.log("winws завершился: " + err.Error())
		} else {
			a.log("winws остановлен")
		}
		a.emit("state", a.state())
	}

	_ = a.w.Bind("__call", a.call)
	a.w.Init(`window.__pending={};window.api=function(m){var a=[].slice.call(arguments,1);var id=Math.random().toString(36).slice(2);
return new Promise(function(res,rej){window.__pending[id]={res:res,rej:rej};window.__call(m,JSON.stringify(a),id);});};
window.__done=function(id,ok,v){var p=window.__pending[id];if(!p)return;delete window.__pending[id];ok?p.res(v):p.rej(v);};`)
	a.w.SetHtml(indexHTML)

	a.tray = NewTray(a.hwnd, iconData, a.trayClick, []TrayItem{
		{"Открыть ZapretGUI", a.show},
		{"Включить / выключить обход", func() { go a.toggle() }},
		{"Следующая стратегия", func() { go func() { _, _ = a.next() }() }},
		{"", nil},
		{"Выход", a.exit},
	})

	a.log(fmt.Sprintf("ZapretGUI %s · файлы Flowseal %s · %s", AppVersion, EmbeddedFlowsealVersion(), DataDir()))
	if extractErr != nil {
		a.log("Внимание: " + extractErr.Error())
	}

	go func() {
		t := time.NewTicker(2 * time.Second)
		for range t.C {
			if a.scanning.Load() {
				continue
			}
			st := a.state()
			a.emit("state", st)
			tip := "ZapretGUI — выключено"
			if st.Running || st.Service.Running {
				tip = "ZapretGUI — обход работает (" + st.Strategy + ")"
			}
			a.tray.SetTip(tip)
		}
	}()
	if a.st.CheckUpdates {
		go func() {
			time.Sleep(3 * time.Second)
			if v, err := LatestFlowsealVersion(); err == nil && v != EmbeddedFlowsealVersion() {
				a.emit("update", v)
				a.log("Вышла новая версия Flowseal: " + v)
			}
		}()
	}
	if a.st.AutoStart {
		go func() {
			if _, err := a.start(a.st.Strategy, true); err != nil {
				a.log("Автозапуск: " + err.Error())
			}
		}()
	}
	if a.st.StartMinimized && a.st.MinimizeToTray {
		pShowWindow.Call(a.hwnd, swHide)
	}
	a.w.Run()
	a.shutdown()
	if a.tray != nil {
		a.tray.Remove()
	}
}

// ---------- окно ----------

func (a *App) styleWindow() {
	on := uint32(1)
	pDwmSetWindowAttr.Call(a.hwnd, 20, uintptr(unsafe.Pointer(&on)), 4) // тёмный заголовок
	color := uint32(0x00140F0B)                                         // #0B0F14 (BGR)
	pDwmSetWindowAttr.Call(a.hwnd, 35, uintptr(unsafe.Pointer(&color)), 4)
	corner := uint32(2)
	pDwmSetWindowAttr.Call(a.hwnd, 33, uintptr(unsafe.Pointer(&corner)), 4)
}

func (a *App) hookClose() {
	cb := windows.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
		if msg == wmClose && !a.quitting.Load() && a.st.MinimizeToTray {
			pShowWindow.Call(hwnd, swHide)
			if a.tray != nil {
				a.tray.Balloon("ZapretGUI работает в трее", "Нажмите на значок, чтобы открыть окно. Выход — через меню значка.")
			}
			return 0
		}
		r, _, _ := pCallWindowProcW.Call(a.oldProc, hwnd, msg, wp, lp)
		return r
	})
	a.oldProc, _, _ = pSetWindowLongPtrW.Call(a.hwnd, gwlpWndProc, cb)
}

func (a *App) show() {
	pShowWindow.Call(a.hwnd, swRestore)
	pShowWindow.Call(a.hwnd, swShow)
	pSetForegroundWindow.Call(a.hwnd)
}

func (a *App) trayClick() {
	if v, _, _ := pIsWindowVisible.Call(a.hwnd); v != 0 {
		pShowWindow.Call(a.hwnd, swHide)
	} else {
		a.show()
	}
}

func (a *App) exit() {
	a.quitting.Store(true)
	a.cancel.Store(true)
	pCallWindowProcW.Call(a.oldProc, a.hwnd, wmClose, 0, 0)
}

func (a *App) shutdown() {
	a.quitting.Store(true)
	a.run.Stop()
	a.waitStopped()
}

// ---------- связь с интерфейсом ----------

func (a *App) emit(ev string, data any) {
	b, _ := json.Marshal(data)
	e, _ := json.Marshal(ev)
	a.w.Dispatch(func() { a.w.Eval("window.onEvent&&window.onEvent(" + string(e) + "," + string(b) + ")") })
}

func (a *App) log(s string) {
	line := time.Now().Format("15:04:05  ") + s
	a.mu.Lock()
	a.lastLogs = append(a.lastLogs, line)
	if len(a.lastLogs) > 2000 {
		a.lastLogs = a.lastLogs[1000:]
	}
	a.mu.Unlock()
	a.emit("log", line)
}

// call — асинхронный вызов из JS: api("метод", ...аргументы) → Promise.
func (a *App) call(method, argsJSON, id string) {
	go func() {
		var args []json.RawMessage
		_ = json.Unmarshal([]byte(argsJSON), &args)
		str := func(i int) string {
			var s string
			if i < len(args) {
				_ = json.Unmarshal(args[i], &s)
			}
			return s
		}
		boolean := func(i int) bool {
			var b bool
			if i < len(args) {
				_ = json.Unmarshal(args[i], &b)
			}
			return b
		}
		var res any
		var err error
		switch method {
		case "init":
			a.mu.Lock()
			logs := append([]string{}, a.lastLogs...)
			a.mu.Unlock()
			res = map[string]any{"state": a.state(), "logs": logs}
		case "state":
			res = a.state()
		case "start":
			res, err = a.start(str(0), boolean(1))
		case "stop":
			a.run.Stop()
			a.waitStopped()
			res = a.state()
		case "next":
			res, err = a.next()
		case "select":
			res, err = a.selectStrategy(str(0))
		case "installService":
			err = a.installService(str(0))
			res = a.state()
		case "removeService":
			err = RemoveService()
			if err == nil {
				a.log("Служба удалена")
			}
			res = a.state()
		case "saveSettings":
			var s Settings
			if len(args) > 0 {
				err = json.Unmarshal(args[0], &s)
			}
			if err == nil {
				res, err = a.saveSettings(s)
			}
		case "getList":
			res, err = readUserList(str(0))
		case "saveList":
			err = writeUserList(str(0), str(1))
			if err == nil {
				a.log("Список сохранён: " + str(0))
				res = a.run.Running()
			}
		case "ping":
			res = QuickPing()
		case "scan":
			err = a.scan(boolean(0))
		case "cancelScan":
			a.cancel.Store(true)
		case "updateIPSet":
			var n int
			n, err = UpdateIPSetFromGitHub()
			if err == nil {
				if a.st.IPSetMode == "loaded" {
					_ = ApplyIPSetMode("loaded")
				}
				a.log(fmt.Sprintf("Список IP обновлён (%d строк)", n))
				res = n
			}
		case "clearDiscord":
			rep := ClearDiscordCache()
			for _, r := range rep {
				a.log(r)
			}
			res = rep
		case "checkUpdates":
			var v string
			v, err = LatestFlowsealVersion()
			res = map[string]string{"latest": v, "local": EmbeddedFlowsealVersion()}
		case "command":
			res = "winws.exe " + strings.Join(a.argsFor(a.current()), " ")
		case "license":
			b, _ := assetsFS.ReadFile("assets/LICENSE-Flowseal.txt")
			res = string(b)
		case "open":
			u := str(0)
			if u == "folder" {
				u = DataDir()
			}
			if strings.HasPrefix(u, "https://") || u == DataDir() {
				OpenURL(u)
			}
		default:
			err = errors.New("неизвестный метод " + method)
		}
		ok := err == nil
		var payload []byte
		if ok {
			payload, _ = json.Marshal(res)
		} else {
			payload, _ = json.Marshal(err.Error())
		}
		idj, _ := json.Marshal(id)
		okj := "false"
		if ok {
			okj = "true"
		}
		a.w.Dispatch(func() { a.w.Eval("window.__done(" + string(idj) + "," + okj + "," + string(payload) + ")") })
	}()
}

// ---------- логика ----------

type State struct {
	Running      bool       `json:"running"`
	Scanning     bool       `json:"scanning"`
	Strategy     string     `json:"strategy"`
	Strategies   []string   `json:"strategies"`
	Service      SvcInfo    `json:"service"`
	Settings     Settings   `json:"settings"`
	Version      string     `json:"version"`
	Flowseal     string     `json:"flowseal"`
	DataDir      string     `json:"dataDir"`
	ForeignWinws int        `json:"foreignWinws"`
	Lists        []ListInfo `json:"lists"`
}

type ListInfo struct {
	File  string `json:"file"`
	Title string `json:"title"`
}

var userLists = []ListInfo{
	{"list-general-user.txt", "Свои домены"},
	{"list-exclude-user.txt", "Исключения: домены"},
	{"ipset-exclude-user.txt", "Исключения: IP"},
}

func (a *App) state() State {
	a.mu.Lock()
	st := a.st
	a.mu.Unlock()
	names := make([]string, len(a.strats))
	for i, s := range a.strats {
		names[i] = s.Name
	}
	running := a.run.Running()
	own := -1
	if running {
		own = a.run.Pid()
	}
	return State{
		Running: running, Scanning: a.scanning.Load(), Strategy: st.Strategy, Strategies: names,
		Service: QueryService(), Settings: st, Version: AppVersion, Flowseal: EmbeddedFlowsealVersion(),
		DataDir: DataDir(), ForeignWinws: len(ForeignWinws(own)), Lists: userLists,
	}
}

func (a *App) find(name string) int {
	for i, s := range a.strats {
		if s.Name == name {
			return i
		}
	}
	return -1
}

func (a *App) current() Strategy {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := a.find(a.st.Strategy); i >= 0 {
		return a.strats[i]
	}
	return a.strats[0]
}

func (a *App) argsFor(s Strategy) []string {
	a.mu.Lock()
	gf := a.st.Game()
	a.mu.Unlock()
	return BuildArgs(s, BinDir(), ListsDir(), gf)
}

func (a *App) waitStopped() {
	for i := 0; i < 60 && a.run.Running(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
}

func (a *App) prepare() {
	_ = ExtractAssets()
	a.mu.Lock()
	ipset, ts := a.st.IPSetMode, a.st.TCPTimestamps
	a.mu.Unlock()
	_ = ApplyIPSetMode(ipset)
	if ts {
		EnableTCPTimestamps()
	}
}

func (a *App) setStrategy(name string) {
	a.mu.Lock()
	a.st.Strategy = name
	_ = SaveSettings(a.st)
	a.mu.Unlock()
}

// start: force=true — закрыть чужие winws (запущенные через .bat Flowseal).
func (a *App) start(name string, force bool) (State, error) {
	if a.scanning.Load() {
		return a.state(), errors.New("идёт подбор стратегии")
	}
	i := a.find(name)
	if i < 0 {
		return a.state(), errors.New("стратегия не найдена")
	}
	if QueryService().Running {
		return a.state(), errors.New("service")
	}
	a.run.Stop()
	a.waitStopped()
	if others := ForeignWinws(-1); len(others) > 0 {
		if !force {
			return a.state(), errors.New("foreign")
		}
		KillAllWinws()
		time.Sleep(400 * time.Millisecond)
	}
	a.prepare()
	a.setStrategy(name)
	a.log("Запуск стратегии «" + name + "»")
	if err := a.run.Start(a.argsFor(a.strats[i])); err != nil {
		a.log("Ошибка запуска: " + err.Error())
		return a.state(), fmt.Errorf("не удалось запустить winws.exe: %v. Проверьте, не удалил ли его антивирус (%s)", err, BinDir())
	}
	return a.state(), nil
}

func (a *App) toggle() {
	if a.run.Running() {
		a.run.Stop()
		a.waitStopped()
	} else if _, err := a.start(a.current().Name, false); err != nil {
		a.log("Не удалось включить: " + err.Error())
	}
	a.emit("state", a.state())
}

func (a *App) next() (State, error) {
	i := (a.find(a.current().Name) + 1) % len(a.strats)
	return a.start(a.strats[i].Name, false)
}

func (a *App) selectStrategy(name string) (State, error) {
	if a.find(name) < 0 {
		return a.state(), errors.New("стратегия не найдена")
	}
	if a.run.Running() {
		return a.start(name, false)
	}
	a.setStrategy(name)
	return a.state(), nil
}

func (a *App) installService(name string) error {
	i := a.find(name)
	if i < 0 {
		return errors.New("стратегия не найдена")
	}
	a.run.Stop()
	a.waitStopped()
	KillAllWinws()
	a.prepare()
	if err := InstallService(name, a.argsFor(a.strats[i])); err != nil {
		a.log("Ошибка установки службы: " + err.Error())
		return err
	}
	a.mu.Lock()
	a.st.ServiceStrategy, a.st.Strategy = name, name
	_ = SaveSettings(a.st)
	a.mu.Unlock()
	a.log("Служба установлена: " + name)
	return nil
}

func (a *App) saveSettings(s Settings) (map[string]any, error) {
	s.GameTCP = strings.ReplaceAll(s.GameTCP, " ", "")
	s.GameUDP = strings.ReplaceAll(s.GameUDP, " ", "")
	if !ValidPortRange(s.GameTCP) || !ValidPortRange(s.GameUDP) {
		return nil, errors.New("неверный формат портов, пример: 1024-1934,1936-65535")
	}
	a.mu.Lock()
	old := a.st
	s.Strategy, s.ServiceStrategy = old.Strategy, old.ServiceStrategy
	a.st = s
	err := SaveSettings(s)
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	_ = ApplyIPSetMode(s.IPSetMode)
	changed := old.GameMode != s.GameMode || old.GameTCP != s.GameTCP || old.GameUDP != s.GameUDP || old.IPSetMode != s.IPSetMode
	return map[string]any{
		"needRestart":       changed && a.run.Running(),
		"serviceNeedsReset": changed && QueryService().Installed,
	}, nil
}

func listPath(name string) (string, error) {
	for _, l := range userLists {
		if l.File == name {
			return filepath.Join(ListsDir(), name), nil
		}
	}
	return "", errors.New("неизвестный список")
}

func readUserList(name string) (string, error) {
	p, err := listPath(name)
	if err != nil {
		return "", err
	}
	b, _ := os.ReadFile(p)
	return strings.ReplaceAll(string(b), "\r\n", "\n"), nil
}

func writeUserList(name, text string) error {
	p, err := listPath(name)
	if err != nil {
		return err
	}
	t := strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if t == "" { // пустой файл ломает winws (см. Flowseal)
		t = "domain.example.abc"
		if name == "ipset-exclude-user.txt" {
			t = ipsetDummy
		}
	}
	return os.WriteFile(p, []byte(strings.ReplaceAll(t, "\n", "\r\n")+"\r\n"), 0644)
}

// scan перебирает все стратегии и меряет доступность и задержку YouTube/Discord.
func (a *App) scan(full bool) error {
	if !a.scanning.CompareAndSwap(false, true) {
		return errors.New("подбор уже идёт")
	}
	defer a.scanning.Store(false)
	if QueryService().Running {
		return errors.New("работает служба автозапуска — удалите её на главной, чтобы подбирать стратегию")
	}
	a.cancel.Store(false)
	wasRunning := a.run.Running()
	prev := a.current().Name
	a.run.Stop()
	a.waitStopped()
	KillAllWinws()
	a.prepare()
	a.log("Подбор стратегии: старт (" + map[bool]string{true: "полный", false: "быстрый"}[full] + " режим)")

	total := len(a.strats) + 1
	var results []ScanResult
	a.emit("scan", map[string]any{"type": "start", "total": total})

	// 1) без обхода — чтобы было видно, что именно блокируется
	a.emit("scan", map[string]any{"type": "progress", "index": 0, "total": total, "name": "Без обхода"})
	base := Summarize("Без обхода", RunChecks(full))
	base.Baseline = true
	results = append(results, base)
	a.emit("scan", map[string]any{"type": "result", "result": base})

	for i, s := range a.strats {
		if a.cancel.Load() {
			break
		}
		a.emit("scan", map[string]any{"type": "progress", "index": i + 1, "total": total, "name": s.Name})
		if err := a.run.Start(a.argsFor(s)); err != nil {
			a.log("Подбор: не удалось запустить " + s.Name + ": " + err.Error())
			continue
		}
		time.Sleep(1500 * time.Millisecond) // winws поднимает WinDivert
		r := Summarize(s.Name, RunChecks(full))
		a.run.Stop()
		a.waitStopped()
		results = append(results, r)
		a.emit("scan", map[string]any{"type": "result", "result": r})
		a.log(fmt.Sprintf("Подбор: %-26s %d/%d, %d мс", s.Name, r.OK, r.Total, r.Ms))
	}
	SortResults(results)
	best := ""
	if len(results) > 0 && !results[0].Baseline && results[0].OK > 0 {
		best = results[0].Strategy
	}
	cancelled := a.cancel.Load()
	a.scanning.Store(false)
	if wasRunning && !a.quitting.Load() {
		_, _ = a.start(prev, true)
	}
	a.log("Подбор завершён. Лучшая: " + orDash(best))
	a.emit("scan", map[string]any{"type": "done", "best": best, "results": results, "cancelled": cancelled})
	a.emit("state", a.state())
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
