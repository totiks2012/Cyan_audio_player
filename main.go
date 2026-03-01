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
)

const (
	stateFile    = ".cyan_state.json"
	configFile   = "config.json"
	socketPath   = "/tmp/cyan.sock"
	m3uSeparator = "|#|"
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
			if m.curDur > 0 && m.curPos >= m.curDur-1.0 {
				m.nextTrack()
			}
		}
		return m, tea.Tick(time.Second/2, func(t time.Time) tea.Msg { return time.Time(t) })

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
			oldDir := filepath.Base(m.state.Cwd)
			m.state.Cwd = filepath.Dir(m.state.Cwd)
			m.refresh()
			for i, item := range m.fmItems {
				if item.name == oldDir {
					m.fmCur = i
					break
				}
			}
		case " ":
			_, _ = m.player.SendCommand([]interface{}{"cycle", "pause"})
		case ",":
			_, _ = m.player.SendCommand([]interface{}{"seek", -5})
		case ".":
			_, _ = m.player.SendCommand([]interface{}{"seek", 5})
		}
		m.sync()

	case tea.WindowSizeMsg:
		m.termWidth, m.termHeight = msg.Width, msg.Height
		m.height = msg.Height - 11
		if m.height < 5 {
			m.height = 5
		}
	}
	return m, nil
}

func (m *model) changeVolume(delta int) {
	m.state.Volume += delta
	if m.state.Volume < 0 {
		m.state.Volume = 0
	}
	if m.state.Volume > 100 {
		m.state.Volume = 100
	}
	_, _ = m.player.SendCommand([]interface{}{"set_property", "volume", m.state.Volume})
	m.save()
}

func (m *model) View() string { return RenderUI(m) }

// playTrack извлекает чистый URL/путь из записи плейлиста и запускает mpv
func (m *model) playTrack(idx int) {
	if idx < 0 || idx >= len(m.state.Playlist) {
		return
	}
	raw := m.state.Playlist[idx]
	path := raw
	if strings.Contains(raw, m3uSeparator) {
		parts := strings.SplitN(raw, m3uSeparator, 2)
		path = parts[1]
	}
	_ = m.player.Start(path, m.state.Volume)
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
			if it.isDir {
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
			m.playTrack(m.state.CurrentIndex)
		}
	}
}

func isAudio(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	audioExts := map[string]bool{
		".mp3": true, ".flac": true, ".wav": true, ".ogg": true,
		".m4a": true, ".opus": true, ".aac": true, ".wma": true,
	}
	return audioExts[ext]
}

func isM3U(filename string) bool {
	return strings.ToLower(filepath.Ext(filename)) == ".m3u"
}

func (m *model) add() {
	if len(m.fmItems) == 0 || m.fmCur >= len(m.fmItems) {
		return
	}
	it := m.fmItems[m.fmCur]

	if !it.isDir && isM3U(it.name) {
		m.state.Playlist = []string{}
		m.state.CurrentIndex = -1
		m.plCur, m.plOff = 0, 0

		file, err := os.Open(it.path)
		if err == nil {
			scanner := bufio.NewScanner(file)
			var currentName string
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				if strings.HasPrefix(line, "#EXTINF:-1,") {
					namePart := strings.TrimPrefix(line, "#EXTINF:-1,")
					// Проверяем, что это имя, а не URL
					if !strings.Contains(namePart, "http://") && !strings.Contains(namePart, "https://") {
						currentName = namePart
					}
				} else if !strings.HasPrefix(line, "#") {
					// Если это URL/адрес радио
					if currentName != "" {
						m.state.Playlist = append(m.state.Playlist, currentName+m3uSeparator+line)
						currentName = "" // сброс после использования
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
	} else {
		if isAudio(it.name) {
			m.state.Playlist = append(m.state.Playlist, it.path)
		}
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
	e, _ := os.ReadDir(m.state.Cwd)
	for _, x := range e {
		abs, _ := filepath.Abs(filepath.Join(m.state.Cwd, x.Name()))
		m.fmItems = append(m.fmItems, displayItem{abs, x.Name(), x.IsDir()})
	}
	sort.Slice(m.fmItems, func(i, j int) bool {
		if m.fmItems[i].isDir != m.fmItems[j].isDir {
			return m.fmItems[i].isDir
		}
		return strings.ToLower(m.fmItems[i].name) < strings.ToLower(m.fmItems[j].name)
	})

	m.plItems = nil
	for _, raw := range m.state.Playlist {
		name := filepath.Base(raw)
		path := raw
		if strings.Contains(raw, m3uSeparator) {
			parts := strings.SplitN(raw, m3uSeparator, 2)
			name = parts[0]
			path = parts[1]
		}
		m.plItems = append(m.plItems, displayItem{path, name, false})
	}
	m.sync()
}

func (m *model) doSearch() {
	if m.searchInput == "" {
		m.refresh()
		return
	}
	m.refresh()
	var filtered []displayItem
	items := m.fmItems
	if m.focus == 1 {
		items = m.plItems
	}
	searchLower := strings.ToLower(m.searchInput)
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.name), searchLower) {
			filtered = append(filtered, it)
		}
	}
	if m.focus == 0 {
		m.fmItems = filtered
	} else {
		m.plItems = filtered
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

func main() {
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
	m := &model{state: st, config: cfg, player: NewPlayer(socketPath), styles: InitStyles(cfg), height: 20}
	m.refresh()
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		os.Exit(1)
	}
}