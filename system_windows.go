package main

// Работа с Windows: распаковка файлов, запуск/остановка winws.exe,
// служба автозапуска (совместима со службой "zapret" из service.bat Flowseal).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	AppName     = "ZapretGUI"
	AppVersion  = "2.0.0"
	ServiceName = "zapret"
	ipsetDummy  = "203.0.113.113/32"
)

// ---------- пути и настройки ----------

func DataDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, AppName)
}
func BinDir() string   { return filepath.Join(DataDir(), "bin") }
func ListsDir() string { return filepath.Join(DataDir(), "lists") }
func WinwsPath() string {
	return filepath.Join(BinDir(), "winws.exe")
}

type Settings struct {
	Strategy        string `json:"strategy"`
	GameMode        string `json:"game_mode"`
	GameTCP         string `json:"game_tcp"`
	GameUDP         string `json:"game_udp"`
	IPSetMode       string `json:"ipset_mode"` // none | any | loaded
	TCPTimestamps   bool   `json:"tcp_timestamps"`
	AutoStart       bool   `json:"auto_start"`
	MinimizeToTray  bool   `json:"minimize_to_tray"`
	StartMinimized  bool   `json:"start_minimized"`
	CheckUpdates    bool   `json:"check_updates"`
	ServiceStrategy string `json:"service_strategy"`
	Accent          string `json:"accent"`
	ScanFull        bool   `json:"scan_full"`
}

func DefaultSettings() Settings {
	return Settings{
		Strategy: "general", GameMode: "disabled", GameTCP: "1024-65535", GameUDP: "1024-65535",
		IPSetMode: "none", TCPTimestamps: true, MinimizeToTray: true, CheckUpdates: true, Accent: "mint",
	}
}

func settingsPath() string { return filepath.Join(DataDir(), "settings.json") }

func LoadSettings() Settings {
	s := DefaultSettings()
	if b, err := os.ReadFile(settingsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func SaveSettings(s Settings) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(settingsPath(), b, 0644)
}

func (s Settings) Game() GameFilter {
	return GameFilter{Mode: s.GameMode, TCPRange: s.GameTCP, UDPRange: s.GameUDP}
}

// ---------- распаковка ----------

// ExtractAssets раскладывает встроенные файлы Flowseal в %ProgramData%\ZapretGUI.
// Пользовательские списки (*-user.txt) никогда не перезаписываются.
func ExtractAssets() error {
	if err := os.MkdirAll(BinDir(), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(ListsDir(), 0755); err != nil {
		return err
	}
	var errs []string
	for _, sub := range []string{"bin", "lists"} {
		dst := filepath.Join(DataDir(), sub)
		_ = fs.WalkDir(assetsFS, "assets/"+sub, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, _ := assetsFS.ReadFile(p)
			target := filepath.Join(dst, d.Name())
			if old, err := os.ReadFile(target); err == nil && bytes.Equal(old, data) {
				return nil
			}
			if err := os.WriteFile(target, data, 0644); err != nil {
				// файл может быть занят работающим winws — не критично, если он уже есть
				if _, statErr := os.Stat(target); statErr != nil {
					errs = append(errs, d.Name())
				}
			}
			return nil
		})
	}
	ensure := func(name, content string) {
		p := filepath.Join(ListsDir(), name)
		if _, err := os.Stat(p); err != nil {
			_ = os.WriteFile(p, []byte(content), 0644)
		}
	}
	ensure("ipset-exclude-user.txt", ipsetDummy+"\r\n")
	ensure("list-general-user.txt", "# Never leave this file empty\r\ndomain.example.abc\r\n")
	ensure("list-exclude-user.txt", "domain.example.abc\r\n")
	if _, err := os.Stat(filepath.Join(ListsDir(), "ipset-all.txt")); err != nil {
		_ = ApplyIPSetMode("none")
	}
	if len(errs) > 0 {
		return fmt.Errorf("не удалось записать: %s", strings.Join(errs, ", "))
	}
	return nil
}

// ApplyIPSetMode — аналог пункта "IPSet Filter" в service.bat.
func ApplyIPSetMode(mode string) error {
	var content []byte
	switch mode {
	case "any":
		content = []byte{}
	case "loaded":
		full, err := os.ReadFile(filepath.Join(ListsDir(), "ipset-all.full"))
		if err != nil {
			full, _ = assetsFS.ReadFile("assets/lists/ipset-all.full")
		}
		content = full
	default:
		content = []byte(ipsetDummy + "\r\n")
	}
	return os.WriteFile(filepath.Join(ListsDir(), "ipset-all.txt"), content, 0644)
}

// UpdateIPSetFromGitHub скачивает свежий список IP из репозитория Flowseal.
func UpdateIPSetFromGitHub() (int, error) {
	const url = "https://raw.githubusercontent.com/Flowseal/zapret-discord-youtube/refs/heads/main/.service/ipset-service.txt"
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return 0, err
	}
	n := bytes.Count(data, []byte("\n"))
	if n < 10 {
		return 0, errors.New("получен пустой список")
	}
	return n, os.WriteFile(filepath.Join(ListsDir(), "ipset-all.full"), data, 0644)
}

// ---------- запуск winws ----------

type Runner struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	job  windows.Handle
	Log  func(string)
	Exit func(error)
}

func (r *Runner) Pid() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil && r.cmd.Process != nil {
		return r.cmd.Process.Pid
	}
	return -1
}

func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd != nil
}

func EnableTCPTimestamps() {
	c := exec.Command("netsh", "interface", "tcp", "set", "global", "timestamps=enabled")
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	_ = c.Run()
}

func (r *Runner) Start(args []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil {
		return errors.New("уже запущено")
	}
	cmd := exec.Command(WinwsPath(), args...)
	cmd.Dir = BinDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}
	// Job Object: winws завершится вместе с программой, даже при аварийном закрытии.
	if job, err := windows.CreateJobObject(nil, nil); err == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		_, _ = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
		if h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); err == nil {
			_ = windows.AssignProcessToJobObject(job, h)
			windows.CloseHandle(h)
		}
		r.job = job
	}
	r.cmd = cmd
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if r.Log != nil {
				r.Log(sc.Text())
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		pw.Close()
		r.mu.Lock()
		if r.job != 0 {
			windows.CloseHandle(r.job)
			r.job = 0
		}
		r.cmd = nil
		r.mu.Unlock()
		if r.Exit != nil {
			r.Exit(err)
		}
	}()
	return nil
}

func (r *Runner) Stop() {
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// ---------- процессы ----------

func findProcesses(name string) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []uint32
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			out = append(out, pe.ProcessID)
		}
	}
	return out
}

func killPIDs(pids []uint32) {
	for _, pid := range pids {
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
			_ = windows.TerminateProcess(h, 1)
			windows.CloseHandle(h)
		}
	}
}

func ForeignWinws(own int) []uint32 {
	var out []uint32
	for _, p := range findProcesses("winws.exe") {
		if int(p) != own {
			out = append(out, p)
		}
	}
	return out
}

func KillAllWinws() { killPIDs(findProcesses("winws.exe")) }

// ---------- служба ----------

type SvcInfo struct {
	Installed bool
	Running   bool
	Strategy  string
	Ours      bool
}

func QueryService() SvcInfo {
	var si SvcInfo
	m, err := mgr.Connect()
	if err != nil {
		return si
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return si
	}
	defer s.Close()
	si.Installed = true
	if st, err := s.Query(); err == nil {
		si.Running = st.State == svc.Running || st.State == svc.StartPending
	}
	if cfg, err := s.Config(); err == nil {
		si.Ours = strings.Contains(strings.ToLower(cfg.BinaryPathName), strings.ToLower(DataDir()))
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+ServiceName, registry.QUERY_VALUE); err == nil {
		si.Strategy, _, _ = k.GetStringValue("zapret-discord-youtube")
		k.Close()
	}
	return si
}

func stopAndWait(s *mgr.Service) {
	st, err := s.Control(svc.Stop)
	if err != nil {
		return
	}
	for i := 0; i < 50 && st.State != svc.Stopped; i++ {
		time.Sleep(100 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return
		}
	}
}

func RemoveService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(ServiceName); err == nil {
		stopAndWait(s)
		err = s.Delete()
		s.Close()
		if err != nil {
			return err
		}
	}
	// как в service.bat: выгружаем драйвер WinDivert
	for _, n := range []string{"WinDivert", "WinDivert14"} {
		if s, err := m.OpenService(n); err == nil {
			stopAndWait(s)
			s.Close()
		}
	}
	time.Sleep(300 * time.Millisecond)
	return nil
}

func InstallService(strategy string, args []string) error {
	if err := RemoveService(); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	var s *mgr.Service
	for i := 0; i < 20; i++ { // служба может быть "помечена на удаление" пару секунд
		s, err = m.CreateService(ServiceName, WinwsPath(), mgr.Config{
			DisplayName: "zapret",
			Description: "Zapret DPI bypass software (ZapretGUI, based on Flowseal/zapret-discord-youtube)",
			StartType:   mgr.StartAutomatic,
		}, args...)
		if err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	defer s.Close()
	if k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+ServiceName, registry.SET_VALUE); err == nil {
		_ = k.SetStringValue("zapret-discord-youtube", strategy)
		k.Close()
	}
	return s.Start()
}

// ---------- прочее ----------

func ClearDiscordCache() []string {
	var report []string
	appdata := os.Getenv("APPDATA")
	for _, d := range []struct{ exe, name, dir string }{
		{"Discord.exe", "Discord", "discord"},
		{"DiscordPTB.exe", "Discord PTB", "discordptb"},
		{"DiscordCanary.exe", "Discord Canary", "discordcanary"},
		{"DiscordDevelopment.exe", "Discord Development", "discorddevelopment"},
	} {
		base := filepath.Join(appdata, d.dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		if p := findProcesses(d.exe); len(p) > 0 {
			killPIDs(p)
			report = append(report, d.name+": закрыт")
			time.Sleep(700 * time.Millisecond)
		}
		for _, c := range []string{"Cache", "Code Cache", "GPUCache"} {
			p := filepath.Join(base, c)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if err := os.RemoveAll(p); err != nil {
				report = append(report, d.name+": не удалось удалить "+c)
			} else {
				report = append(report, d.name+": очищен "+c)
			}
		}
	}
	if len(report) == 0 {
		report = append(report, "Discord не найден")
	}
	return report
}

func EmbeddedFlowsealVersion() string {
	b, _ := assetsFS.ReadFile("assets/flowseal-version.txt")
	return strings.TrimSpace(string(b))
}

func LatestFlowsealVersion() (string, error) {
	c := &http.Client{Timeout: 8 * time.Second}
	resp, err := c.Get("https://raw.githubusercontent.com/Flowseal/zapret-discord-youtube/main/.service/version.txt")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(b)), err
}

func OpenURL(u string) {
	_ = windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(u), nil, nil, windows.SW_SHOWNORMAL)
}
