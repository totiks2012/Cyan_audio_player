package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	stateFile     = ".cyan_state.json"
	configFile    = "config.json"
	socketPath    = "/tmp/cyan.sock"
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
		if m.player.running {
			m.curPos, m.curDur = m.player.GetPosition()
			if m.curDur > 0 && m.curPos >= m.curDur-1.0 { m.nextTrack() }
		}
		return m, tea.Tick(time.Second/2, func(t time.Time) tea.Msg { return time.Time(t) })

	case tea.MouseMsg:
		switch msg.Type {
		case tea.MouseLeft:
			if msg.Action == tea.MouseActionPress { m.handleMouse(msg.X, msg.Y) }
		case tea.MouseWheelUp:
			if m.focus == 0 { m.fmCur-- } else { m.plCur-- }
		case tea.MouseWheelDown:
			if m.focus == 0 { m.fmCur++ } else { m.plCur++ }
		}
		m.sync()

	case tea.KeyMsg:
		if m.searchMode {
			switch msg.String() {
			case "enter", "esc": m.searchMode = false
			case "backspace":
				r := []rune(m.searchInput)
				if len(r) > 0 { m.searchInput = string(r[:len(r)-1]); m.doSearch() }
			default:
				if msg.Type == tea.KeyRunes || msg.String() == " " {
					m.searchInput += string(msg.Runes); m.doSearch()
				}
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c": m.save(); m.player.Stop(); return m, tea.Quit
		case "Q": m.save(); m.player.SaveAndStop(); return m, tea.Quit
		case "/": m.searchMode = true; m.searchInput = ""
		case "f2": m.add()
		case "f3": m.remove()
		case "f5": m.clearPlaylist()
		case "tab": m.focus = (m.focus + 1) % 2
		case "up": if m.focus == 0 { m.fmCur-- } else { m.plCur-- }
		case "down": if m.focus == 0 { m.fmCur++ } else { m.plCur++ }
		case "n": m.nextTrack()
		case "-", "_": m.changeVolume(-5)
		case "=", "+": m.changeVolume(5)
		case "enter", "right": m.action()
		case "left": m.goUp()
		case " ": _, _ = m.player.SendCommand([]interface{}{"cycle", "pause"})
		case ",": _, _ = m.player.SendCommand([]interface{}{"seek", -5})
		case ".": _, _ = m.player.SendCommand([]interface{}{"seek", 5})
		}
		m.sync()

	case tea.WindowSizeMsg:
		m.termWidth, m.termHeight = msg.Width, msg.Height
		m.height = msg.Height - 10
		if m.height < 5 { m.height = 5 }
	}
	return m, nil
}

func (m *model) handleMouse(x, y int) {
	// ДИНАМИЧЕСКИЙ РАСЧЕТ:
	// Определяем высоту заголовка каждой панели напрямую через рендерер.
	// Офсет = высота заголовка + 1 (верхняя рамка).
	fmHeaderHeight := lipgloss.Height(RenderFMHeader(m))
	plHeaderHeight := lipgloss.Height(RenderPLHeader(m))

	newFocus := -1
	if x >= 0 && x <= 50 { newFocus = 0 }
	if x >= 51 && x <= 101 { newFocus = 1 }

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
	if idx < 0 || idx >= len(m.state.Playlist) { return }
	raw := m.state.Playlist[idx]
	path := raw
	if strings.Contains(raw, m3uSeparator) {
		path = strings.SplitN(raw, m3uSeparator, 2)[1]
	}
	_ = m.player.Start(path, m.state.Volume)
}

func (m *model) nextTrack() {
	if len(m.state.Playlist) > 0 {
		m.state.CurrentIndex = (m.state.CurrentIndex + 1) % len(m.state.Playlist)
		m.playTrack(m.state.CurrentIndex)
		m.plCur = m.state.CurrentIndex
		m.sync(); m.save()
	}
}

func (m *model) action() {
	if m.focus == 0 {
		if len(m.fmItems) > 0 && m.fmCur < len(m.fmItems) {
			it := m.fmItems[m.fmCur]
			if it.name == ".." {
				m.goUp()
			} else if it.isDir {
				m.state.Cwd = it.path; m.fmCur, m.fmOff = 0, 0; m.refresh()
			} else { m.add() }
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
	return map[string]bool{".mp3":true,".flac":true,".wav":true,".ogg":true,".m4a":true,".opus":true,".aac":true,".wma":true}[e]
}

func (m *model) add() {
	if len(m.fmItems) == 0 || m.fmCur >= len(m.fmItems) { return }
	it := m.fmItems[m.fmCur]
	if it.name == ".." { return }

	if !it.isDir && strings.ToLower(filepath.Ext(it.name)) == ".m3u" {
		m.state.Playlist = []string{}; m.state.CurrentIndex = -1; m.plCur, m.plOff = 0, 0
		if file, err := os.Open(it.path); err == nil {
			scanner := bufio.NewScanner(file)
			var curName string
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" { continue }
				if strings.HasPrefix(line, "#EXTINF:-1,") {
					n := strings.TrimPrefix(line, "#EXTINF:-1,")
					if !strings.HasPrefix(n, "http") { curName = n }
				} else if !strings.HasPrefix(line, "#") {
					if curName != "" { m.state.Playlist = append(m.state.Playlist, curName+m3uSeparator+line); curName = "" } else { m.state.Playlist = append(m.state.Playlist, line) }
				}
			}
			file.Close()
		}
	} else if it.isDir {
		files, _ := os.ReadDir(it.path)
		for _, f := range files {
			if !f.IsDir() && isAudio(f.Name()) { m.state.Playlist = append(m.state.Playlist, filepath.Join(it.path, f.Name())) }
		}
	} else if isAudio(it.name) { m.state.Playlist = append(m.state.Playlist, it.path) }
	m.refresh(); m.save()
}

func (m *model) remove() {
	if len(m.state.Playlist) > 0 && m.plCur < len(m.state.Playlist) {
		m.state.Playlist = append(m.state.Playlist[:m.plCur], m.state.Playlist[m.plCur+1:]...)
		if m.state.CurrentIndex == m.plCur { m.state.CurrentIndex = -1 }
		m.refresh(); m.save()
	}
}

func (m *model) clearPlaylist() {
	m.state.Playlist = []string{}; m.state.CurrentIndex = -1; m.plCur, m.plOff = 0, 0
	m.refresh(); m.save()
}

func (m *model) refresh() {
	m.fmItems = nil
	m.fmItems = append(m.fmItems, displayItem{filepath.Dir(m.state.Cwd), "..", true})

	e, _ := os.ReadDir(m.state.Cwd)
	var d, f []displayItem
	for _, x := range e {
		abs, _ := filepath.Abs(filepath.Join(m.state.Cwd, x.Name()))
		it := displayItem{abs, x.Name(), x.IsDir()}
		if x.IsDir() { d = append(d, it) } else { f = append(f, it) }
	}
	sort.Slice(d, func(i, j int) bool { return strings.ToLower(d[i].name) < strings.ToLower(d[j].name) })
	sort.Slice(f, func(i, j int) bool { return strings.ToLower(f[i].name) < strings.ToLower(f[j].name) })
	m.fmItems = append(m.fmItems, d...)
	m.fmItems = append(m.fmItems, f...)

	m.plItems = nil
	for _, raw := range m.state.Playlist {
		n, p := filepath.Base(raw), raw
		if strings.Contains(raw, m3uSeparator) {
			parts := strings.SplitN(raw, m3uSeparator, 2); n, p = parts[0], parts[1]
		}
		m.plItems = append(m.plItems, displayItem{p, n, false})
	}
	m.sync()
}

func (m *model) changeVolume(delta int) {
	m.state.Volume += delta
	if m.state.Volume < 0 { m.state.Volume = 0 } else if m.state.Volume > 100 { m.state.Volume = 100 }
	_, _ = m.player.SendCommand([]interface{}{"set_property", "volume", m.state.Volume})
	m.save()
}

func (m *model) View() string { return RenderUI(m) }

func (m *model) doSearch() {
	m.refresh()
	items := m.fmItems; if m.focus == 1 { items = m.plItems }
	var res []displayItem
	s := strings.ToLower(m.searchInput)
	for _, it := range items { if strings.Contains(strings.ToLower(it.name), s) { res = append(res, it) } }
	if m.focus == 0 { m.fmItems = res } else { m.plItems = res }
	m.fmCur, m.plCur = 0, 0
	m.sync()
}

func (m *model) sync() {
	if m.fmCur < 0 { m.fmCur = 0 } else if len(m.fmItems) > 0 && m.fmCur >= len(m.fmItems) { m.fmCur = len(m.fmItems)-1 }
	if m.plCur < 0 { m.plCur = 0 } else if len(m.plItems) > 0 && m.plCur >= len(m.plItems) { m.plCur = len(m.plItems)-1 }
	if m.fmCur < m.fmOff { m.fmOff = m.fmCur } else if m.fmCur >= m.fmOff+m.height { m.fmOff = m.fmCur - m.height + 1 }
	if m.plCur < m.plOff { m.plOff = m.plCur } else if m.plCur >= m.plOff+m.height { m.plOff = m.plCur - m.height + 1 }
}

func (m *model) save() { d, _ := json.Marshal(m.state); _ = os.WriteFile(stateFile, d, 0644) }

func main() {
	cfg := Config{ThemeColor: "#00FFFF", BgCursor: "#005555", BorderStyle: "rounded"}
	if d, err := os.ReadFile(configFile); err == nil { _ = json.Unmarshal(d, &cfg) }
	st := State{Volume: 50, CurrentIndex: -1}
	if d, err := os.ReadFile(stateFile); err == nil { _ = json.Unmarshal(d, &st) }
	if st.Cwd == "" { st.Cwd, _ = os.Getwd() }
	m := &model{state: st, config: cfg, player: NewPlayer(socketPath), styles: InitStyles(cfg), height: 20}
	m.refresh()
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil { os.Exit(1) }
}