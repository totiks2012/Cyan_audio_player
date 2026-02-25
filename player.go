package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

type MPVPlayer struct {
	socket  string
	running bool
}

func NewPlayer(socketPath string) *MPVPlayer {
	return &MPVPlayer{socket: socketPath}
}

func (p *MPVPlayer) Start(path string, volume int) error {
	p.Stop()
	cmd := exec.Command("mpv", "--idle", "--input-ipc-server="+p.socket, "--no-video", fmt.Sprintf("--volume=%d", volume), path)
	if err := cmd.Start(); err != nil { return err }
	p.running = true
	time.Sleep(200 * time.Millisecond)
	return nil
}

func (p *MPVPlayer) Stop() {
	_, _ = p.SendCommand([]interface{}{"quit"})
	_ = os.Remove(p.socket)
	p.running = false
}

func (p *MPVPlayer) SaveAndStop() {
	_, _ = p.SendCommand([]interface{}{"write-watch-later-config"})
	p.Stop()
}

func (p *MPVPlayer) SendCommand(args []interface{}) (map[string]interface{}, error) {
	conn, err := net.Dial("unix", p.socket)
	if err != nil { return nil, err }
	defer conn.Close()

	cmd := map[string]interface{}{"command": args}
	data, _ := json.Marshal(cmd)
	conn.Write(append(data, '\n'))

	var resp map[string]interface{}
	_ = json.NewDecoder(conn).Decode(&resp)
	return resp, nil
}

func (p *MPVPlayer) GetPosition() (float64, float64) {
	posResp, _ := p.SendCommand([]interface{}{"get_property", "time-pos"})
	durResp, _ := p.SendCommand([]interface{}{"get_property", "duration"})
	
	var pos, dur float64
	if v, ok := posResp["data"].(float64); ok { pos = v }
	if v, ok := durResp["data"].(float64); ok { dur = v }
	return pos, dur
}