package main

// Значок в трее на чистом WinAPI (сообщения обрабатываются циклом WebView2 в том же потоке).

import (
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pShellNotifyIconW   = shell32.NewProc("Shell_NotifyIconW")
	pRegisterClassExW   = user32.NewProc("RegisterClassExW")
	pCreateWindowExW    = user32.NewProc("CreateWindowExW")
	pDefWindowProcW     = user32.NewProc("DefWindowProcW")
	pCreatePopupMenu    = user32.NewProc("CreatePopupMenu")
	pAppendMenuW        = user32.NewProc("AppendMenuW")
	pTrackPopupMenu     = user32.NewProc("TrackPopupMenu")
	pDestroyMenu        = user32.NewProc("DestroyMenu")
	pGetCursorPos       = user32.NewProc("GetCursorPos")
	pLoadImageW         = user32.NewProc("LoadImageW")
	pGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	pRegisterWindowMsgW = user32.NewProc("RegisterWindowMessageW")
	pPostMessageW       = user32.NewProc("PostMessageW")
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     uintptr
}

type wndClassEx struct {
	Size, Style                        uint32
	WndProc                            uintptr
	ClsExtra, WndExtra                 int32
	Instance, Icon, Cursor, Background uintptr
	MenuName, ClassName                *uint16
	IconSm                             uintptr
}

type TrayItem struct {
	Text string
	Fn   func()
}

type Tray struct {
	hwnd    uintptr
	nid     notifyIconData
	items   []TrayItem
	click   func()
	created uint32
}

const (
	wmTray      = 0x8000 + 1
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	nifMessage  = 1
	nifIcon     = 2
	nifTip      = 4
	nifInfo     = 0x10
)

func copyUTF16(dst []uint16, s string) {
	u, _ := windows.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)-1]
		u = append(u, 0)
	}
	copy(dst, u)
}

func NewTray(_ uintptr, ico []byte, click func(), items []TrayItem) *Tray {
	t := &Tray{items: items, click: click}
	cls, _ := windows.UTF16PtrFromString("ZapretGUITray")
	var inst windows.Handle
	_ = windows.GetModuleHandleEx(0, nil, &inst)
	wc := wndClassEx{WndProc: windows.NewCallback(t.proc), Instance: uintptr(inst), ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	t.hwnd, _, _ = pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(cls)), 0, 0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if t.hwnd == 0 {
		return t
	}
	tb, _ := windows.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := pRegisterWindowMsgW.Call(uintptr(unsafe.Pointer(tb)))
	t.created = uint32(r)

	p := filepath.Join(DataDir(), "tray.ico")
	_ = os.WriteFile(p, ico, 0644)
	pp, _ := windows.UTF16PtrFromString(p)
	cx, _, _ := pGetSystemMetrics.Call(49) // SM_CXSMICON
	cy, _, _ := pGetSystemMetrics.Call(50)
	icon, _, _ := pLoadImageW.Call(0, uintptr(unsafe.Pointer(pp)), 1, cx, cy, 0x10) // LR_LOADFROMFILE

	t.nid.CbSize = uint32(unsafe.Sizeof(t.nid))
	t.nid.HWnd = t.hwnd
	t.nid.UID = 1
	t.nid.UFlags = nifMessage | nifIcon | nifTip
	t.nid.UCallbackMessage = wmTray
	t.nid.HIcon = icon
	copyUTF16(t.nid.SzTip[:], "ZapretGUI")
	t.add()
	return t
}

func (t *Tray) add() {
	n := t.nid
	n.UFlags = nifMessage | nifIcon | nifTip
	pShellNotifyIconW.Call(0, uintptr(unsafe.Pointer(&n)))
}

func (t *Tray) SetTip(s string) {
	if t == nil || t.hwnd == 0 {
		return
	}
	copyUTF16(t.nid.SzTip[:], s)
	n := t.nid
	n.UFlags = nifTip
	pShellNotifyIconW.Call(1, uintptr(unsafe.Pointer(&n)))
}

var balloonShown bool

func (t *Tray) Balloon(title, text string) {
	if t == nil || t.hwnd == 0 || balloonShown {
		return
	}
	balloonShown = true
	n := t.nid
	n.UFlags = nifInfo
	copyUTF16(n.SzInfoTitle[:], title)
	copyUTF16(n.SzInfo[:], text)
	n.DwInfoFlags = 1 // NIIF_INFO
	pShellNotifyIconW.Call(1, uintptr(unsafe.Pointer(&n)))
}

func (t *Tray) Remove() {
	if t == nil || t.hwnd == 0 {
		return
	}
	n := t.nid
	pShellNotifyIconW.Call(2, uintptr(unsafe.Pointer(&n)))
}

func (t *Tray) proc(hwnd, msg, wp, lp uintptr) uintptr {
	switch {
	case msg == wmTray:
		switch uint32(lp) & 0xFFFF {
		case wmLButtonUp:
			if t.click != nil {
				t.click()
			}
		case wmRButtonUp:
			t.menu()
		}
		return 0
	case t.created != 0 && uint32(msg) == t.created:
		t.add() // проводник перезапустился
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func (t *Tray) menu() {
	m, _, _ := pCreatePopupMenu.Call()
	for i, it := range t.items {
		if it.Text == "" {
			pAppendMenuW.Call(m, 0x800, 0, 0) // MF_SEPARATOR
			continue
		}
		s, _ := windows.UTF16PtrFromString(it.Text)
		pAppendMenuW.Call(m, 0, uintptr(i+1), uintptr(unsafe.Pointer(s)))
	}
	var pt struct{ X, Y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(m, 0x0100|0x0080|0x0008|0x0020, uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0) // RETURNCMD|NONOTIFY|RIGHTALIGN
	pDestroyMenu.Call(m)
	pPostMessageW.Call(t.hwnd, 0, 0, 0)
	if cmd > 0 && int(cmd) <= len(t.items) && t.items[cmd-1].Fn != nil {
		t.items[cmd-1].Fn()
	}
}
