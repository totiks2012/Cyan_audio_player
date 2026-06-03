package main

/*
#cgo LDFLAGS: -lmpv
#include <mpv/client.h>
#include <stdlib.h>

int mpv_cmd_string(mpv_handle *ctx, const char *arg1, const char *arg2) {
	const char *cmd[] = {arg1, arg2, NULL};
	return mpv_command(ctx, cmd);
}
*/
import "C"
import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const stateFileSuffix = ".cyan_player_state"
const sessionFileName = ".cy_pl_state"

func sessionDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "cy")
}

func loadSession() string {
	sDir := sessionDir()
	if sDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(sDir, sessionFileName))
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(data))
	if dir == "" {
		return ""
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

func saveSession(dir string) {
	sDir := sessionDir()
	if sDir == "" {
		return
	}
	os.MkdirAll(sDir, 0755)
	os.WriteFile(filepath.Join(sDir, sessionFileName), []byte(dir+"\n"), 0644)
}

type PlayerState struct {
	mu           sync.RWMutex
	CurrentDir   string
	CurrentTrack string
	Position     float64
	Volume       int
}

func (p *PlayerState) save() {
	p.mu.RLock()
	track := p.CurrentTrack
	pos := p.Position
	dir := p.CurrentDir
	p.mu.RUnlock()
	saveSession(dir)
	if track == "" {
		return
	}
	statePath := filepath.Join(dir, stateFileSuffix)
	file, err := os.Create(statePath)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s\n%.2f\n", track, pos)
}

func loadConfig() map[string]string {
	cfg := make(map[string]string)
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}
	p := filepath.Join(home, ".config", "fzi", "config")
	data, err := os.ReadFile(p)
	if err != nil {
		return cfg
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if key != "" {
			cfg[key] = value
		}
	}
	return cfg
}
func loadState(dir string) (track string, pos float64) {
	statePath := filepath.Join(dir, stateFileSuffix)
	file, err := os.Open(statePath)
	if err != nil {
		return "", 0
	}
	defer file.Close()
	s := bufio.NewScanner(file)
	if s.Scan() {
		track = s.Text()
	}
	if s.Scan() {
		pos, _ = strconv.ParseFloat(s.Text(), 64)
	}
	if track == "" {
		return "", 0
	}
	if filepath.Dir(track) != dir {
		return "", 0
	}
	fi, err := os.Stat(track)
	if err != nil || fi.IsDir() {
		return "", 0
	}
	return track, pos
}

type MpvEngine struct {
	mpv      *C.mpv_handle
	cmdChan  chan func()
	endChan  chan struct{}
	loadChan chan struct{}
	stopChan chan struct{}
	closed   int32
	wg       sync.WaitGroup
	posRaw   uint64
	volRaw   int64
}

func NewMpvEngine() *MpvEngine {
	ctx := C.mpv_create()
	if ctx == nil {
		panic("mpv_create failed")
	}
	if res := C.mpv_initialize(ctx); res < 0 {
		panic("mpv_initialize failed")
	}
	cNo := C.CString("no")
	C.mpv_request_log_messages(ctx, cNo)
	C.free(unsafe.Pointer(cNo))

	cPos := C.CString("time-pos")
	cVol := C.CString("volume")
	C.mpv_observe_property(ctx, 10, cPos, C.MPV_FORMAT_DOUBLE)
	C.mpv_observe_property(ctx, 20, cVol, C.MPV_FORMAT_INT64)
	C.free(unsafe.Pointer(cPos))
	C.free(unsafe.Pointer(cVol))

	return &MpvEngine{
		mpv:      ctx,
		cmdChan:  make(chan func(), 32),
		endChan:  make(chan struct{}, 1),
		loadChan: make(chan struct{}, 1),
		stopChan: make(chan struct{}),
	}
}

func (e *MpvEngine) getFloat(name string) float64 {
	if atomic.LoadInt32(&e.closed) == 1 {
		return 0
	}
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	var val C.double
	C.mpv_get_property(e.mpv, cName, C.MPV_FORMAT_DOUBLE, unsafe.Pointer(&val))
	return float64(val)
}

func (e *MpvEngine) getInt(name string) int {
	if atomic.LoadInt32(&e.closed) == 1 {
		return 0
	}
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	var val C.int64_t
	C.mpv_get_property(e.mpv, cName, C.MPV_FORMAT_INT64, unsafe.Pointer(&val))
	return int(val)
}

func (e *MpvEngine) Start() {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			event := C.mpv_wait_event(e.mpv, -1)
			switch event.event_id {
			case C.MPV_EVENT_SHUTDOWN:
				return
			case C.MPV_EVENT_FILE_LOADED:
				select {
				case e.loadChan <- struct{}{}:
				default:
				}
			case C.MPV_EVENT_END_FILE:
				ef := (*C.mpv_event_end_file)(event.data)
				if ef.reason == C.MPV_END_FILE_REASON_EOF {
					select {
					case e.endChan <- struct{}{}:
					default:
					}
				}
			case C.MPV_EVENT_PROPERTY_CHANGE:
				prop := (*C.mpv_event_property)(event.data)
				if prop.data == nil {
					continue
				}
				if event.reply_userdata == 10 {
					val := *(*C.double)(prop.data)
					atomic.StoreUint64(&e.posRaw, math.Float64bits(float64(val)))
				} else if event.reply_userdata == 20 {
					val := *(*C.int64_t)(prop.data)
					atomic.StoreInt64(&e.volRaw, int64(val))
				}
			}
		}
	}()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			select {
			case fn := <-e.cmdChan:
				if atomic.LoadInt32(&e.closed) == 1 {
					return
				}
				fn()
			case <-e.stopChan:
				return
			}
		}
	}()
}

func (e *MpvEngine) Do(fn func()) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return
	}
	select {
	case e.cmdChan <- fn:
	case <-e.stopChan:
	}
}

func (e *MpvEngine) Close() {
	if atomic.CompareAndSwapInt32(&e.closed, 0, 1) {
		close(e.stopChan)
		C.mpv_terminate_destroy(e.mpv)
		e.wg.Wait()
	}
}

func isAudioFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp3", ".flac", ".wav", ".ogg", ".m4a", ".aac", ".opus":
		return true
	}
	return false
}

type DirEntry struct {
	Display string
	Path    string
	IsDir   bool
}

func buildList(dir, currentTrack string) []DirEntry {
	var items []DirEntry
	if dir != "/" {
		items = append(items, DirEntry{
			Display: "🔙 ..",
			Path:    filepath.Dir(dir),
			IsDir:   true,
		})
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return items
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		fullPath := filepath.Join(dir, name)
		if e.IsDir() {
			items = append(items, DirEntry{
				Display: "📁 " + name,
				Path:    fullPath,
				IsDir:   true,
			})
		} else if isAudioFile(name) {
			prefix := "🎵"
			if fullPath == currentTrack {
				prefix = "▶"
			}
			items = append(items, DirEntry{
				Display: prefix + " " + name,
				Path:    fullPath,
			})
		} else if isPlaylist(name) {
			items = append(items, DirEntry{
				Display: "📋 " + name,
				Path:    fullPath,
			})
		}
	}
	return items
}

func fuzzyMatch(name, filter string) bool {
	lower := strings.ToLower(name)
	fi := 0
	for ni := 0; ni < len(lower) && fi < len(filter); ni++ {
		if lower[ni] == filter[fi] {
			fi++
		}
	}
	return fi == len(filter)
}

func isPlaylist(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".m3u" || ext == ".m3u8"
}

type M3UEntry struct {
	Name string
	URL  string
}

func parseM3U(path string) []M3UEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var entries []M3UEntry
	var pendingName string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			rest := line[len("#EXTINF:"):]
			if idx := strings.IndexByte(rest, ','); idx >= 0 {
				pendingName = strings.TrimSpace(rest[idx+1:])
			}
		} else if !strings.HasPrefix(line, "#") {
			url := line
			if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "/") {
				url = filepath.Join(filepath.Dir(path), url)
			}
			name := pendingName
			if name == "" {
				name = filepath.Base(url)
			}
			entries = append(entries, M3UEntry{Name: name, URL: url})
			pendingName = ""
		}
	}
	return entries
}

func parseColor(s string) tcell.Color {
	s = strings.TrimSpace(s)
	if len(s) == 0 || s[0] != '#' {
		switch strings.ToLower(s) {
		case "black":
			return tcell.ColorBlack
		case "white":
			return tcell.ColorWhite
		case "red":
			return tcell.ColorRed
		case "green":
			return tcell.ColorGreen
		case "yellow":
			return tcell.ColorYellow
		case "blue":
			return tcell.ColorBlue
		case "teal":
			return tcell.ColorTeal
		case "aqua":
			return tcell.ColorAqua
		case "gray", "grey":
			return tcell.ColorGray
		case "purple":
			return tcell.ColorPurple
		}
		return tcell.ColorDefault
	}
	s = s[1:]
	if len(s) == 3 {
		s = string(s[0]) + string(s[0]) + string(s[1]) + string(s[1]) + string(s[2]) + string(s[2])
	}
	if len(s) != 6 {
		return tcell.ColorDefault
	}
	r, _ := strconv.ParseInt(s[0:2], 16, 32)
	g, _ := strconv.ParseInt(s[2:4], 16, 32)
	b, _ := strconv.ParseInt(s[4:6], 16, 32)
	return tcell.NewRGBColor(int32(r), int32(g), int32(b))
}

func main() {
	os.Setenv("PIPEWIRE_DEBUG", "0")

	dir := filepath.Join(os.Getenv("HOME"), "Music")
	if len(os.Args) > 1 {
		dir = os.Args[1]
	} else {
		if d := loadSession(); d != "" {
			dir = d
		}
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	saveSession(absDir)

	savedTrack, savedPos := loadState(absDir)

	player := &PlayerState{
		CurrentDir:   absDir,
		CurrentTrack: savedTrack,
	}

	engine := NewMpvEngine()
	engine.Start()

	app := tview.NewApplication()

	var telemetryStop chan struct{}
	var telemetryMu sync.Mutex

	cfg := loadConfig()

	input := tview.NewInputField().
		SetLabel("🔍 fzi> ").
		SetFieldWidth(0)
	if v, ok := cfg["input_bg"]; ok {
		input.SetFieldBackgroundColor(parseColor(v))
	} else {
		input.SetFieldBackgroundColor(tcell.ColorBlack)
	}
	if v, ok := cfg["input_text"]; ok {
		input.SetFieldTextColor(parseColor(v))
	} else {
		input.SetFieldTextColor(tcell.ColorWhite)
	}
	if v, ok := cfg["label_color"]; ok {
		input.SetLabelColor(parseColor(v))
	} else {
		input.SetLabelColor(tcell.ColorYellow)
	}

	list := tview.NewList()
	list.SetHighlightFullLine(true)
	list.ShowSecondaryText(false)
	if v, ok := cfg["selected_bg"]; ok {
		list.SetSelectedBackgroundColor(parseColor(v))
	} else {
		list.SetSelectedBackgroundColor(tcell.ColorBlue)
	}
	if v, ok := cfg["selected_text"]; ok {
		list.SetSelectedTextColor(parseColor(v))
	} else {
		list.SetSelectedTextColor(tcell.ColorWhite)
	}

	statusBar := tview.NewTextView()
	statusBar.SetDynamicColors(true)
	if v, ok := cfg["status_text"]; ok {
		statusBar.SetTextColor(parseColor(v))
	} else {
		statusBar.SetTextColor(tcell.ColorYellow)
	}
	if v, ok := cfg["status_bg"]; ok {
		statusBar.SetBackgroundColor(parseColor(v))
	}
	statusBar.SetText("▶ CY Player")

	stopTel := func() {
		telemetryMu.Lock()
		if telemetryStop != nil {
			close(telemetryStop)
			telemetryStop = nil
		}
		telemetryMu.Unlock()
	}

	startTel := func() {
		ch := make(chan struct{})
		telemetryMu.Lock()
		telemetryStop = ch
		telemetryMu.Unlock()
		go func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					pos := math.Float64frombits(atomic.LoadUint64(&engine.posRaw))
					vol := int(atomic.LoadInt64(&engine.volRaw))
					player.mu.Lock()
					player.Position = pos
					player.Volume = vol
					player.mu.Unlock()
					select {
					case <-ch:
						return
					default:
					}
					app.QueueUpdateDraw(func() {
						player.mu.RLock()
						track := filepath.Base(player.CurrentTrack)
						min := int(player.Position) / 60
						sec := int(player.Position) % 60
						text := fmt.Sprintf("Pos: %d:%02d | Vol: %d%%", min, sec, player.Volume)
						if track != "" && track != "." {
							text = track + " | " + text
						}
						player.mu.RUnlock()
						statusBar.SetText(text)
					})
				case <-ch:
					return
				}
			}
		}()
	}

	startTel()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		stopTel()
		player.save()
		app.Stop()
	}()

	var entries []DirEntry
	var filtered []DirEntry
	var m3uEntries []M3UEntry
	var browsingM3U bool

	rebuild := func(filter string) {
		list.Clear()
		if browsingM3U {
			var shown []M3UEntry
			shown = m3uEntries
			if filter != "" {
				lower := strings.ToLower(filter)
				var f []M3UEntry
				for _, e := range shown {
					if fuzzyMatch(e.Name, lower) {
						f = append(f, e)
					}
				}
				shown = f
			}
			for _, e := range shown {
				list.AddItem(e.Name, "", 0, nil)
			}
			return
		}
		player.mu.RLock()
		dir := player.CurrentDir
		track := player.CurrentTrack
		player.mu.RUnlock()
		entries = buildList(dir, track)
		filtered = entries
		if filter != "" {
			lowerFilter := strings.ToLower(filter)
			var f []DirEntry
			for _, e := range filtered {
				if fuzzyMatch(e.Display, lowerFilter) {
					f = append(f, e)
				}
			}
			filtered = f
		}
		for _, e := range filtered {
			list.AddItem(e.Display, "", 0, nil)
		}
	}

	rebuild("")

	handleSelect := func() {
		idx := list.GetCurrentItem()
		if idx < 0 {
			return
		}
		if browsingM3U {
			if idx >= len(m3uEntries) {
				return
			}
			entry := m3uEntries[idx]
			player.mu.Lock()
			player.CurrentTrack = entry.URL
			player.mu.Unlock()
			engine.Do(func() {
				cLoad := C.CString("loadfile")
				cTrack := C.CString(entry.URL)
				C.mpv_cmd_string(engine.mpv, cLoad, cTrack)
				C.free(unsafe.Pointer(cLoad))
				C.free(unsafe.Pointer(cTrack))
			})
			return
		}
		if idx >= len(filtered) {
			return
		}
		e := filtered[idx]
		if e.IsDir {
			player.mu.Lock()
			player.CurrentDir = e.Path
			player.mu.Unlock()
			browsingM3U = false
			rebuild(input.GetText())
			return
		}
		ext := strings.ToLower(filepath.Ext(e.Path))
		if ext == ".m3u" || ext == ".m3u8" {
			entries2 := parseM3U(e.Path)
			if len(entries2) > 0 {
				m3uEntries = entries2
				browsingM3U = true
				rebuild("")
			}
			return
		}
		player.mu.Lock()
		player.CurrentTrack = e.Path
		player.mu.Unlock()
		path := e.Path
		engine.Do(func() {
			cLoad := C.CString("loadfile")
			cTrack := C.CString(path)
			C.mpv_cmd_string(engine.mpv, cLoad, cTrack)
			C.free(unsafe.Pointer(cLoad))
			C.free(unsafe.Pointer(cTrack))
		})
		rebuild(input.GetText())
	}

	input.SetChangedFunc(func(text string) {
		rebuild(text)
	})

	flex := tview.NewFlex().SetDirection(tview.FlexRow)
	flex.AddItem(input, 3, 0, true)
	flex.AddItem(list, 0, 1, false)
	flex.AddItem(statusBar, 1, 0, false)

	flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyUp:
			if list.GetItemCount() > 0 {
				idx := list.GetCurrentItem()
				if idx > 0 {
					list.SetCurrentItem(idx - 1)
				}
			}
			return nil
		case tcell.KeyDown:
			if list.GetItemCount() > 0 {
				idx := list.GetCurrentItem()
				if idx < list.GetItemCount()-1 {
					list.SetCurrentItem(idx + 1)
				}
			}
			return nil
		case tcell.KeyEnter:
			handleSelect()
			return nil
		case tcell.KeyEscape:
			if browsingM3U {
				browsingM3U = false
				m3uEntries = nil
				rebuild(input.GetText())
				return nil
			}
			stopTel()
			app.Stop()
			return nil
		case tcell.KeyCtrlZ:
			app.Suspend(func() {
				syscall.Kill(syscall.Getpid(), syscall.SIGTSTP)
			})
			return nil
		case tcell.KeyCtrlP:
			engine.Do(func() {
				cProp := C.CString("pause")
				var cur C.int
				C.mpv_get_property(engine.mpv, cProp, C.MPV_FORMAT_FLAG, unsafe.Pointer(&cur))
				var next C.int = 1
				if cur == 1 {
					next = 0
				}
				C.mpv_set_property(engine.mpv, cProp, C.MPV_FORMAT_FLAG, unsafe.Pointer(&next))
				C.free(unsafe.Pointer(cProp))
			})
			return nil
		case tcell.KeyCtrlQ:
			stopTel()
			player.save()
			app.Stop()
			return nil
		case tcell.KeyCtrlU:
			input.SetText("")
			return nil
		default:
			if event.Key() == tcell.KeyRune {
				switch event.Rune() {
				case '-':
					engine.Do(func() {
						cProp := C.CString("volume")
						cur := engine.getInt("volume")
						next := C.int64_t(cur - 5)
						if next < 0 {
							next = 0
						}
						C.mpv_set_property(engine.mpv, cProp, C.MPV_FORMAT_INT64, unsafe.Pointer(&next))
						C.free(unsafe.Pointer(cProp))
					})
					return nil
				case '=', '+':
					engine.Do(func() {
						cProp := C.CString("volume")
						cur := engine.getInt("volume")
						next := C.int64_t(cur + 5)
						if next > 130 {
							next = 130
						}
						C.mpv_set_property(engine.mpv, cProp, C.MPV_FORMAT_INT64, unsafe.Pointer(&next))
						C.free(unsafe.Pointer(cProp))
					})
					return nil
				case '[':
					engine.Do(func() {
						cSeek := C.CString("seek")
						cVal := C.CString("-5")
						C.mpv_cmd_string(engine.mpv, cSeek, cVal)
						C.free(unsafe.Pointer(cSeek))
						C.free(unsafe.Pointer(cVal))
					})
					return nil
				case ']':
					engine.Do(func() {
						cSeek := C.CString("seek")
						cVal := C.CString("5")
						C.mpv_cmd_string(engine.mpv, cSeek, cVal)
						C.free(unsafe.Pointer(cSeek))
						C.free(unsafe.Pointer(cVal))
					})
					return nil
				case 'd':
					if event.Modifiers()&tcell.ModAlt != 0 {
						os.Remove(filepath.Join(player.CurrentDir, stateFileSuffix))
						os.Remove(filepath.Join(sessionDir(), sessionFileName))
						player.mu.Lock()
						player.CurrentTrack = ""
						player.Position = 0
						player.mu.Unlock()
						statusBar.SetText("Стейт сброшен, сорцы забыты")
						return nil
					}
				}
			}
		}
		return event
	})

	if savedTrack != "" {
		fi, err := os.Stat(savedTrack)
		if err == nil && !fi.IsDir() {
			player.mu.Lock()
			player.CurrentTrack = savedTrack
			player.mu.Unlock()

			engine.Do(func() {
				cLoad := C.CString("loadfile")
				cTrack := C.CString(savedTrack)
				C.mpv_cmd_string(engine.mpv, cLoad, cTrack)
				C.free(unsafe.Pointer(cLoad))
				C.free(unsafe.Pointer(cTrack))
			})

			select {
			case <-engine.loadChan:
				engine.Do(func() {
					cSeek := C.CString("seek")
					cVal := C.CString(fmt.Sprintf("%.2f", savedPos))
					C.mpv_cmd_string(engine.mpv, cSeek, cVal)
					C.free(unsafe.Pointer(cSeek))
					C.free(unsafe.Pointer(cVal))
				})
			case <-time.After(3 * time.Second):
			}
			rebuild("")
		}
	}

	go func() {
		for range engine.endChan {
			if browsingM3U {
				continue
			}
			player.mu.RLock()
			dir := player.CurrentDir
			track := player.CurrentTrack
			player.mu.RUnlock()
			if track == "" {
				continue
			}

			all := buildList(dir, "")
			var tracks []string
			curIdx := -1
			for _, e := range all {
				if !e.IsDir {
					tracks = append(tracks, e.Path)
					if e.Path == track {
						curIdx = len(tracks) - 1
					}
				}
			}
			if len(tracks) == 0 || (len(tracks) == 1 && curIdx == 0) {
				continue
			}
			nextIdx := 0
			if curIdx >= 0 && curIdx < len(tracks)-1 {
				nextIdx = curIdx + 1
			}
			nextTrack := tracks[nextIdx]

			player.mu.Lock()
			player.CurrentTrack = nextTrack
			player.mu.Unlock()

			engine.Do(func() {
				cLoad := C.CString("loadfile")
				cTrack := C.CString(nextTrack)
				C.mpv_cmd_string(engine.mpv, cLoad, cTrack)
				C.free(unsafe.Pointer(cLoad))
				C.free(unsafe.Pointer(cTrack))
			})

			app.QueueUpdateDraw(func() {
				rebuild(input.GetText())
			})
		}
	}()

	app.SetRoot(flex, true).EnableMouse(true)
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}

	stopTel()
	done := make(chan struct{})
	go func() {
		engine.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
	}
}
