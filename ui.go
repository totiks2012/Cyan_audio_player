package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

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
	if cfg.BorderStyle == "double" { border = lipgloss.DoubleBorder() }
	return UIStyles{
		Box:    lipgloss.NewStyle().Border(border).BorderForeground(lipgloss.Color("#333333")).Width(50),
		Active: lipgloss.NewStyle().Border(border).BorderForeground(theme).Width(50),
		Head:   lipgloss.NewStyle().Background(theme).Foreground(lipgloss.Color("#000000")).Bold(true).Padding(0, 1),
		Cursor: lipgloss.NewStyle().Background(lipgloss.Color(cfg.BgCursor)).Foreground(lipgloss.Color("#FFFFFF")),
		Help:   lipgloss.NewStyle().Foreground(lipgloss.Color("#666666")),
		Neon:   lipgloss.NewStyle().Foreground(theme),
	}
}

func RenderUI(m *model) string {
	lS, rS := m.styles.Box, m.styles.Box
	if m.focus == 0 { lS = m.styles.Active } else { rS = m.styles.Active }

	fV := m.styles.Head.Render(" FILES ") + "\n"
	for i := m.fmOff; i < m.fmOff+m.height && i < len(m.fmItems); i++ {
		icon := "○ "; if m.fmItems[i].isDir { icon = "◆ " }
		line := icon + TrimText(m.fmItems[i].name, 35)
		if i == m.fmCur && m.focus == 0 { fV += m.styles.Cursor.Render(line) + "\n" } else { fV += line + "\n" }
	}

	pV := m.styles.Head.Render(" PLAYLIST ") + "\n"
	for i := m.plOff; i < m.plOff+m.height && i < len(m.plItems); i++ {
		isPlaying := m.state.CurrentIndex == i && m.player.running
		pref := "  "; if isPlaying { pref = "> " }
		line := pref + TrimText(m.plItems[i].name, 35)
		style := lipgloss.NewStyle()
		if m.state.CurrentIndex == i { style = style.Underline(true) }
		if i == m.plCur && m.focus == 1 { pV += m.styles.Cursor.Render(line) + "\n" } else {
			if isPlaying { style = m.styles.Neon.Underline(true) }
			pV += style.Render(line) + "\n"
		}
	}

	help := m.styles.Help.Render("TAB: focus | ENTER/>: enter | -/+: volume | /: search | N: next")
	if m.searchMode { help = m.styles.Neon.Render("SEARCH: " + m.searchInput) }

	barWidth := 50
	bar := RenderProgressBar(barWidth, m.curPos, m.curDur, lipgloss.Color(m.config.ThemeColor))
	timer := fmt.Sprintf(" %02d:%02d / %02d:%02d", int(m.curPos)/60, int(m.curPos)%60, int(m.curDur)/60, int(m.curDur)%60)
	vol := m.styles.Neon.Render(fmt.Sprintf(" VOL: %d%%", m.state.Volume))

	content := lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top, lS.Height(m.height+1).Render(fV), rS.Height(m.height+1).Render(pV)),
		"\n "+bar+timer+vol, "\n "+help)

	return lipgloss.Place(m.termWidth, m.termHeight, lipgloss.Center, lipgloss.Center, content)
}

func TrimText(s string, w int) string {
	r := []rune(s)
	if len(r) <= w { return s }
	return string(r[:w-3]) + "..."
}

func RenderProgressBar(width int, current, total float64, color lipgloss.Color) string {
	if total <= 0 { return lipgloss.NewStyle().Foreground(lipgloss.Color("#333333")).Render(strings.Repeat("─", width)) }
	ratio := current / total
	if ratio > 1 { ratio = 1 }
	filled := int(ratio * float64(width))
	return lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("━", filled)) +
		lipgloss.NewStyle().Foreground(lipgloss.Color("#333333")).Render(strings.Repeat("─", width-filled))
}