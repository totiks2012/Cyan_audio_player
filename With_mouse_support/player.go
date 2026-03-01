// player.go
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type MPVPlayer struct {
	socketPath string
	running    bool
	cmd        *exec.Cmd
}

func NewPlayer(socket string) *MPVPlayer {
	return &MPVPlayer{socketPath: socket}
}

func (p *MPVPlayer) Start(path string, vol int) error {
	p.Stop()
	_ = os.Remove(p.socketPath)

	// Создаем локальную папку для истории, если её нет
	historyPath, _ := filepath.Abs("./.cyan_history")
	_ = os.MkdirAll(historyPath, 0755)

	args := []string{
		"--no-terminal", "--no-video", "--vo=null", "--no-config",
		fmt.Sprintf("--input-ipc-server=%s", p.socketPath),
		fmt.Sprintf("--volume=%d", vol),
		"--save-position-on-quit=yes",
		fmt.Sprintf("--watch-later-directory=%s", historyPath),
		"--", path,
	}

	p.cmd = exec.Command("mpv", args...)
	if err := p.cmd.Start(); err != nil {
		return err
	}
	p.running = true
	go func() { _ = p.cmd.Wait(); p.running = false }()
	
	time.Sleep(200 * time.Millisecond)
	return nil
}

func (p *MPVPlayer) Stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = exec.Command("pkill", "-9", "mpv").Run()
	p.running = false
	_ = os.Remove(p.socketPath)
}

func (p *MPVPlayer) SaveAndStop() {
	if p.running {
		// Принудительная запись конфига через IPC
		_, _ = p.SendCommand([]interface{}{"write-watch-later-config"})
		// Даем полсекунды на физическую запись файла на диск
		time.Sleep(500 * time.Millisecond)
	}
	p.Stop()
}

func (p *MPVPlayer) SendCommand(cmd []interface{}) (string, error) {
	conn, err := net.DialTimeout("unix", p.socketPath, 100*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	payload, _ := json.Marshal(map[string]interface{}{"command": cmd})
	_, _ = conn.Write(append(payload, '\n'))
	buf := make([]byte, 1024)
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	n, err := conn.Read(buf)
	if err != nil { return "", err }
	return string(buf[:n]), nil
}

func (p *MPVPlayer) GetPosition() (float64, float64) {
	if !p.running { return 0, 0 }
	rP, _ := p.SendCommand([]interface{}{"get_property", "time-pos"})
	rD, _ := p.SendCommand([]interface{}{"get_property", "duration"})
	var dP, dD struct{ Data float64 `json:"data"` }
	_ = json.Unmarshal([]byte(rP), &dP)
	_ = json.Unmarshal([]byte(resToData(rD)), &dD) // Вспомогательный фикс для парсинга
	
	// Если mpv вернул ошибку или пустой ответ, используем старые данные
	if dP.Data < 0 { dP.Data = 0 }
	return dP.Data, dD.Data
}

// Вспомогательная очистка JSON от mpv
func resToData(s string) string {
	if s == "" { return `{"data":0}` }
	return s
}