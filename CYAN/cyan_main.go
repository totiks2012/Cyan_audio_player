package main

/*
#cgo pkg-config: mpv
#include <mpv/client.h>
#include <stdlib.h>
*/
import "C"

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	stateFile     = ".cyan_state.json"
	configFile    = "config.json"
	doubleClickMs = 400
	m3uSeparator  = "|#|"
)

type Config struct {
	ThemeColor  string `json:"theme_color"`
	BgCursor    string `json:"bg_cursor"`
	BorderStyle string `json:"border_style"`
}

type State struct {
	Cwd          string   `json:"cwd"`
	Playlist     []string `json:"playlist"`
	CurrentIndex int      `json:"current_index"`
	Volume       int      `json:"volume"`
}

type displayItem struct {
	path, name string
	isDir      bool
}

type UIStyles struct {
	Box    lipgloss.Style
	Active lipgloss.Style
	Head   lipgloss.Style
	Cursor lipgloss.Style
	Help   lipgloss.Style
	Neon   lipgloss.Style
}

func InitStyles(cfg Config) UIStyles {
	theme := lipgloss.Color(cfg.ThemeColor)
	border := lipgloss.RoundedBorder()
	if cfg.BorderStyle == "double" {
		border = lipgloss.DoubleBorder()
	}
	return UIStyles{
		Box:    lipgloss.NewStyle().Border(border).BorderForeground(lipgloss.Color("#333333")).Width(50),
		Active: lipgloss.NewStyle().Border(border).BorderForeground(theme).Width(50),
		Head:   lipgloss.NewStyle().Background(theme).Foreground(lipgloss.Color("#000000")).Bold(true).Padding(0, 1),
		Cursor: lipgloss.NewStyle().Background(lipgloss.Color(cfg.BgCursor)).Foreground(lipgloss.Color("#FFFFFF")),
		Help:   lipgloss.NewStyle().Foreground(lipgloss.Color("#666666")),
		Neon:   lipgloss.NewStyle().Foreground(theme),
	}
}

type MPVPlayer struct {
	ctx     *C.mpv_handle
	running bool
	mu      sync.Mutex
}

func NewPlayer() *MPVPlayer {
	p := &MPVPlayer{}
	ctx := C.mpv_create()
	if ctx == nil {
		return nil
	}
	p.ctx = ctx

	p.setOpt("terminal", "no")
	p.setOpt("video", "no")
	p.setOpt("vo", "null")
	p.setOpt("save-position-on-quit", "yes")
	p.setOpt("really-quiet", "yes")

	historyPath, _ := filepath.Abs("./.cyan_history")
	_ = os.MkdirAll(historyPath, 0755)
	p.setOpt("watch-later-directory", historyPath)

	if int(C.mpv_initialize(p.ctx)) < 0 {
		C.mpv_terminate_destroy(p.ctx)
		return nil
	}
	{
		cLevel := C.CString("no")
		C.mpv_request_log_messages(p.ctx, cLevel)
		C.free(unsafe.Pointer(cLevel))
	}

	p.running = true
	return p
}

func (p *MPVPlayer) setOpt(name, val string) {
	cn := C.CString(name)
	cv := C.CString(val)
	C.mpv_set_option_string(p.ctx, cn, cv)
	C.free(unsafe.Pointer(cn))
	C.free(unsafe.Pointer(cv))
}

func (p *MPVPlayer) setProp(name, val string) int {
	cn := C.CString(name)
	cv := C.CString(val)
	r := int(C.mpv_set_property_string(p.ctx, cn, cv))
	C.free(unsafe.Pointer(cn))
	C.free(unsafe.Pointer(cv))
	return r
}

func (p *MPVPlayer) getProp(name string) string {
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	res := C.mpv_get_property_string(p.ctx, cn)
	if res == nil {
		return ""
	}
	defer C.mpv_free(unsafe.Pointer(res))
	return C.GoString(res)
}

func (p *MPVPlayer) Command(args ...string) int {
	if len(args) == 0 {
		return -1
	}
	cargs := make([]*C.char, len(args)+1)
	for i, s := range args {
		cargs[i] = C.CString(s)
	}
	cargs[len(args)] = nil
	defer func() {
		for _, cp := range cargs {
			if cp != nil {
				C.free(unsafe.Pointer(cp))
			}
		}
	}()
	return int(C.mpv_command(p.ctx, &cargs[0]))
}

func (p *MPVPlayer) Start(path string, vol int) {
	p.Command("loadfile", path, "replace")
	p.setProp("volume", fmt.Sprintf("%d", vol))
}

func (p *MPVPlayer) GetPosition() (float64, float64) {
	p.mu.Lock()
	if !p.running || p.ctx == nil {
		p.mu.Unlock()
		return 0, 0
	}
	p.mu.Unlock()

	posS := p.getProp("time-pos")
	durS := p.getProp("duration")

	pos, _ := parseFloat(posS)
	dur, _ := parseFloat(durS)
	if pos < 0 {
		pos = 0
	}
	return pos, dur
}

func (p *MPVPlayer) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx != nil {
		C.mpv_terminate_destroy(p.ctx)
		p.ctx = nil
	}
	p.running = false
}

func (p *MPVPlayer) SaveAndStop() {
	if p.ctx != nil {
		p.Command("write-watch-later-config")
	}
	p.Stop()
}

type model struct {
	state          State
	config         Config
	player         *MPVPlayer
	styles         UIStyles
	fmItems        []displayItem
	plItems        []displayItem
	fmCur, plCur   int
	fmOff, plOff   int
	focus, height  int
	termWidth      int
	termHeight     int
	curPos, curDur float64
	searchMode     bool
	searchInput    string
	lastClick      time.Time
	lastItem       int
	lastFocus      int
}

func (m *model) Init() tea.Cmd {
	if m.state.CurrentIndex >= 0 && m.state.CurrentIndex < len(m.state.Playlist) {
		m.playTrack(m.state.CurrentIndex)
	}
	return tea.Tick(time.Second/2, func(t time.Time) tea.Msg { return time.Time(t) })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case time.Time:
		if m.player != nil {
			m.curPos, m.curDur = m.player.GetPosition()
			if m.curDur > 0 && m.curPos >= m.curDur-1.0 {
				m.nextTrack()
			}
		}
		return m, tea.Tick(time.Second/2, func(t time.Time) tea.Msg { return time.Time(t) })

	case tea.MouseMsg:
		switch msg.Type {
		case tea.MouseLeft:
			if msg.Action == tea.MouseActionPress {
				m.handleMouse(msg.X, msg.Y)
			}
		case tea.MouseWheelUp:
			if m.focus == 0 {
				m.fmCur--
			} else {
				m.plCur--
			}
		case tea.MouseWheelDown:
			if m.focus == 0 {
				m.fmCur++
			} else {
				m.plCur++
			}
		}
		m.sync()

	case tea.KeyMsg:
		if m.searchMode {
			switch msg.String() {
			case "enter", "esc":
				m.searchMode = false
			case "backspace":
				r := []rune(m.searchInput)
				if len(r) > 0 {
					m.searchInput = string(r[:len(r)-1])
					m.doSearch()
				}
			default:
				if msg.Type == tea.KeyRunes || msg.String() == " " {
					m.searchInput += string(msg.Runes)
					m.doSearch()
				}
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			m.save()
			m.player.Stop()
			return m, tea.Quit
		case "Q":
			m.save()
			m.player.SaveAndStop()
			return m, tea.Quit
		case "/":
			m.searchMode = true
			m.searchInput = ""
		case "f2":
			m.add()
		case "f3":
			m.remove()
		case "f5":
			m.clearPlaylist()
		case "tab":
			m.focus = (m.focus + 1) % 2
		case "up":
			if m.focus == 0 {
				m.fmCur--
			} else {
				m.plCur--
			}
		case "down":
			if m.focus == 0 {
				m.fmCur++
			} else {
				m.plCur++
			}
		case "n":
			m.nextTrack()
		case "-", "_":
			m.changeVolume(-5)
		case "=", "+":
			m.changeVolume(5)
		case "enter", "right":
			m.action()
		case "left":
			m.goUp()
		case " ":
			m.player.Command("cycle", "pause")
		case ",":
			m.player.Command("seek", "-5")
		case ".":
			m.player.Command("seek", "5")
		}
		m.sync()

	case tea.WindowSizeMsg:
		m.termWidth, m.termHeight = msg.Width, msg.Height
		m.height = msg.Height - 10
		if m.height < 5 {
			m.height = 5
		}
	}
	return m, nil
}

func (m *model) handleMouse(x, y int) {
	fmHeaderHeight := lipgloss.Height(RenderFMHeader(m))
	plHeaderHeight := lipgloss.Height(RenderPLHeader(m))

	newFocus := -1
	if x >= 0 && x <= 50 {
		newFocus = 0
	}
	if x >= 51 && x <= 101 {
		newFocus = 1
	}

	if newFocus != -1 {
		m.focus = newFocus
		if newFocus == 0 {
			idx := y - fmHeaderHeight - 1
			if idx >= 0 && idx < len(m.fmItems) {
				m.fmCur = idx + m.fmOff
				if time.Since(m.lastClick) < time.Duration(doubleClickMs)*time.Millisecond && m.lastItem == m.fmCur && m.lastFocus == 0 {
					m.action()
				}
				m.lastItem, m.lastFocus = m.fmCur, 0
			}
		} else {
			idxPl := y - plHeaderHeight - 1
			if idxPl >= 0 && idxPl < len(m.plItems) {
				m.plCur = idxPl + m.plOff
				if time.Since(m.lastClick) < time.Duration(doubleClickMs)*time.Millisecond && m.lastItem == m.plCur && m.lastFocus == 1 {
					m.action()
				}
				m.lastItem, m.lastFocus = m.plCur, 1
			}
		}
		m.lastClick = time.Now()
		m.sync()
	}
}

func (m *model) goUp() {
	m.state.Cwd = filepath.Dir(m.state.Cwd)
	m.refresh()
	m.fmCur = 0
}

func (m *model) playTrack(idx int) {
	if idx < 0 || idx >= len(m.state.Playlist) {
		return
	}
	raw := m.state.Playlist[idx]
	path := raw
	if strings.Contains(raw, m3uSeparator) {
		path = strings.SplitN(raw, m3uSeparator, 2)[1]
	}
	m.player.Start(path, m.state.Volume)
}

func (m *model) nextTrack() {
	if len(m.state.Playlist) > 0 {
		m.state.CurrentIndex = (m.state.CurrentIndex + 1) % len(m.state.Playlist)
		m.playTrack(m.state.CurrentIndex)
		m.plCur = m.state.CurrentIndex
		m.sync()
		m.save()
	}
}

func (m *model) action() {
	if m.focus == 0 {
		if len(m.fmItems) > 0 && m.fmCur < len(m.fmItems) {
			it := m.fmItems[m.fmCur]
			if it.name == ".." {
				m.goUp()
			} else if it.isDir {
				m.state.Cwd = it.path
				m.fmCur, m.fmOff = 0, 0
				m.refresh()
			} else {
				m.add()
			}
		}
	} else {
		if len(m.plItems) > 0 && m.plCur < len(m.plItems) {
			m.state.CurrentIndex = m.plCur
			m.playTrack(m.plCur)
		}
	}
}

func isAudio(f string) bool {
	e := strings.ToLower(filepath.Ext(f))
	return map[string]bool{".mp3": true, ".flac": true, ".wav": true, ".ogg": true, ".m4a": true, ".opus": true, ".aac": true, ".wma": true}[e]
}

func (m *model) add() {
	if len(m.fmItems) == 0 || m.fmCur >= len(m.fmItems) {
		return
	}
	it := m.fmItems[m.fmCur]
	if it.name == ".." {
		return
	}

	if !it.isDir && strings.ToLower(filepath.Ext(it.name)) == ".m3u" {
		m.state.Playlist = []string{}
		m.state.CurrentIndex = -1
		m.plCur, m.plOff = 0, 0
		if file, err := os.Open(it.path); err == nil {
			scanner := bufio.NewScanner(file)
			var curName string
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				if strings.HasPrefix(line, "#EXTINF:-1,") {
					n := strings.TrimPrefix(line, "#EXTINF:-1,")
					if !strings.HasPrefix(n, "http") {
						curName = n
					}
				} else if !strings.HasPrefix(line, "#") {
					if curName != "" {
						m.state.Playlist = append(m.state.Playlist, curName+m3uSeparator+line)
						curName = ""
					} else {
						m.state.Playlist = append(m.state.Playlist, line)
					}
				}
			}
			file.Close()
		}
	} else if it.isDir {
		files, _ := os.ReadDir(it.path)
		for _, f := range files {
			if !f.IsDir() && isAudio(f.Name()) {
				m.state.Playlist = append(m.state.Playlist, filepath.Join(it.path, f.Name()))
			}
		}
	} else if isAudio(it.name) {
		m.state.Playlist = append(m.state.Playlist, it.path)
	}
	m.refresh()
	m.save()
}

func (m *model) remove() {
	if len(m.state.Playlist) > 0 && m.plCur < len(m.state.Playlist) {
		m.state.Playlist = append(m.state.Playlist[:m.plCur], m.state.Playlist[m.plCur+1:]...)
		if m.state.CurrentIndex == m.plCur {
			m.state.CurrentIndex = -1
		}
		m.refresh()
		m.save()
	}
}

func (m *model) clearPlaylist() {
	m.state.Playlist = []string{}
	m.state.CurrentIndex = -1
	m.plCur, m.plOff = 0, 0
	m.refresh()
	m.save()
}

func (m *model) refresh() {
	m.fmItems = nil
	m.fmItems = append(m.fmItems, displayItem{filepath.Dir(m.state.Cwd), "..", true})

	e, _ := os.ReadDir(m.state.Cwd)
	var d, f []displayItem
	for _, x := range e {
		abs, _ := filepath.Abs(filepath.Join(m.state.Cwd, x.Name()))
		it := displayItem{abs, x.Name(), x.IsDir()}
		if x.IsDir() {
			d = append(d, it)
		} else {
			f = append(f, it)
		}
	}
	sort.Slice(d, func(i, j int) bool { return strings.ToLower(d[i].name) < strings.ToLower(d[j].name) })
	sort.Slice(f, func(i, j int) bool { return strings.ToLower(f[i].name) < strings.ToLower(f[j].name) })
	m.fmItems = append(m.fmItems, d...)
	m.fmItems = append(m.fmItems, f...)

	m.plItems = nil
	for _, raw := range m.state.Playlist {
		n, p := filepath.Base(raw), raw
		if strings.Contains(raw, m3uSeparator) {
			parts := strings.SplitN(raw, m3uSeparator, 2)
			n, p = parts[0], parts[1]
		}
		m.plItems = append(m.plItems, displayItem{p, n, false})
	}
	m.sync()
}

func (m *model) changeVolume(delta int) {
	m.state.Volume += delta
	if m.state.Volume < 0 {
		m.state.Volume = 0
	} else if m.state.Volume > 100 {
		m.state.Volume = 100
	}
	if m.player != nil {
		m.player.setProp("volume", fmt.Sprintf("%d", m.state.Volume))
	}
	m.save()
}

func (m *model) View() string {
	return RenderUI(m)
}

func (m *model) doSearch() {
	m.refresh()
	items := m.fmItems
	if m.focus == 1 {
		items = m.plItems
	}
	var res []displayItem
	s := strings.ToLower(m.searchInput)
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.name), s) {
			res = append(res, it)
		}
	}
	if m.focus == 0 {
		m.fmItems = res
	} else {
		m.plItems = res
	}
	m.fmCur, m.plCur = 0, 0
	m.sync()
}

func (m *model) sync() {
	if m.fmCur < 0 {
		m.fmCur = 0
	} else if len(m.fmItems) > 0 && m.fmCur >= len(m.fmItems) {
		m.fmCur = len(m.fmItems) - 1
	}
	if m.plCur < 0 {
		m.plCur = 0
	} else if len(m.plItems) > 0 && m.plCur >= len(m.plItems) {
		m.plCur = len(m.plItems) - 1
	}
	if m.fmCur < m.fmOff {
		m.fmOff = m.fmCur
	} else if m.fmCur >= m.fmOff+m.height {
		m.fmOff = m.fmCur - m.height + 1
	}
	if m.plCur < m.plOff {
		m.plOff = m.plCur
	} else if m.plCur >= m.plOff+m.height {
		m.plOff = m.plCur - m.height + 1
	}
}

func (m *model) save() {
	d, _ := json.Marshal(m.state)
	_ = os.WriteFile(stateFile, d, 0644)
}

func RenderFMHeader(m *model) string {
	h := m.styles.Head.Render(" FILES ") + "\n"
	h += m.styles.Help.Render(TrimText(" ◆ "+m.state.Cwd, 48))
	return h
}

func RenderPLHeader(m *model) string {
	h := m.styles.Head.Render(" PLAYLIST ") + "\n"
	h += strings.Repeat(" ", 48)
	return h
}

func RenderUI(m *model) string {
	lS, rS := m.styles.Box, m.styles.Box
	if m.focus == 0 {
		lS = m.styles.Active
	} else {
		rS = m.styles.Active
	}

	fmContent := RenderFMHeader(m) + "\n"
	for i := m.fmOff; i < m.fmOff+m.height && i < len(m.fmItems); i++ {
		it := m.fmItems[i]
		prefix := "  "
		if it.isDir {
			prefix = "◆ "
		}
		line := TrimText(prefix+it.name, 48)
		if i == m.fmCur && m.focus == 0 {
			fmContent += m.styles.Cursor.Render(line) + "\n"
		} else {
			fmContent += line + "\n"
		}
	}

	plContent := RenderPLHeader(m) + "\n"
	for i := m.plOff; i < m.plOff+m.height && i < len(m.plItems); i++ {
		it := m.plItems[i]
		line := TrimText(fmt.Sprintf("%2d. %s", i+1, it.name), 48)
		style := lipgloss.NewStyle()
		isPlaying := (i == m.state.CurrentIndex)
		if isPlaying && i == m.plCur && m.focus == 1 {
			style = style.Underline(true)
		}
		if i == m.plCur && m.focus == 1 {
			plContent += m.styles.Cursor.Render(line) + "\n"
		} else {
			if isPlaying {
				style = m.styles.Neon.Underline(true)
			}
			plContent += style.Render(line) + "\n"
		}
	}

	helpText := "TAB: focus | ARROWS: nav | ENTER: action | . , : seek"
	help := m.styles.Help.Render(helpText)
	if m.searchMode {
		help = m.styles.Neon.Render("SEARCH: " + m.searchInput)
	}

	bar := RenderProgressBar(50, m.curPos, m.curDur, lipgloss.Color(m.config.ThemeColor))
	timer := fmt.Sprintf(" %02d:%02d/%02d:%02d", int(m.curPos)/60, int(m.curPos)%60, int(m.curDur)/60, int(m.curDur)%60)
	vol := m.styles.Neon.Render(fmt.Sprintf(" VOL: %d%%", m.state.Volume))

	return lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top, lS.Height(m.height+2).Render(fmContent), rS.Height(m.height+2).Render(plContent)),
		"",
		" "+bar+timer+vol,
		" "+help)
}

func TrimText(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s + strings.Repeat(" ", w-len(r))
	}
	return string(r[:w-3]) + "..."
}

func RenderProgressBar(width int, cur, total float64, color lipgloss.Color) string {
	if total <= 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#333333")).Render(strings.Repeat("━", width))
	}
	filledWidth := int((cur / total) * float64(width))
	if filledWidth > width {
		filledWidth = width
	}
	if filledWidth < 0 {
		filledWidth = 0
	}
	filled := lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("━", filledWidth))
	empty := lipgloss.NewStyle().Foreground(lipgloss.Color("#444444")).Render(strings.Repeat("━", width-filledWidth))
	return filled + empty
}

func parseFloat(s string) (float64, error) {
	if s == "" || s == "nan" || s == "inf" {
		return 0, nil
	}
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}

func main() {
	os.Setenv("PIPEWIRE_DEBUG", "0")
	cfg := Config{ThemeColor: "#00FFFF", BgCursor: "#005555", BorderStyle: "rounded"}
	if d, err := os.ReadFile(configFile); err == nil {
		_ = json.Unmarshal(d, &cfg)
	}
	st := State{Volume: 50, CurrentIndex: -1}
	if d, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(d, &st)
	}
	if st.Cwd == "" {
		st.Cwd, _ = os.Getwd()
	}

	player := NewPlayer()
	if player == nil {
		fmt.Fprintln(os.Stderr, "FATAL: failed to create mpv player via libmpv/CGO")
		os.Exit(1)
	}

	m := &model{state: st, config: cfg, player: player, styles: InitStyles(cfg), height: 20}
	m.refresh()
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		os.Exit(1)
	}
}
