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

// RenderFMHeader возвращает заголовок левой панели
func RenderFMHeader(m *model) string {
	h := m.styles.Head.Render(" FILES ") + "\n"
	h += m.styles.Help.Render(TrimText(" ◆ "+m.state.Cwd, 48))
	return h
}

// RenderPLHeader возвращает заголовок правой панели
func RenderPLHeader(m *model) string {
	h := m.styles.Head.Render(" PLAYLIST ") + "\n"
	h += strings.Repeat(" ", 48) // Заполнитель для синхронизации высоты
	return h
}

func RenderUI(m *model) string {
	lS, rS := m.styles.Box, m.styles.Box
	if m.focus == 0 { lS = m.styles.Active } else { rS = m.styles.Active }

	// Сборка содержимого левой панели
	fmContent := RenderFMHeader(m) + "\n"
	for i := m.fmOff; i < m.fmOff+m.height && i < len(m.fmItems); i++ {
		it := m.fmItems[i]
		prefix := "  "
		if it.isDir { prefix = "◆ " }
		line := TrimText(prefix+it.name, 48)
		if i == m.fmCur && m.focus == 0 {
			fmContent += m.styles.Cursor.Render(line) + "\n"
		} else {
			fmContent += line + "\n"
		}
	}

	// Сборка содержимого правой панели
	plContent := RenderPLHeader(m) + "\n"
	for i := m.plOff; i < m.plOff+m.height && i < len(m.plItems); i++ {
		it := m.plItems[i]
		line := TrimText(fmt.Sprintf("%2d. %s", i+1, it.name), 48)
		style := lipgloss.NewStyle()
		isPlaying := (i == m.state.CurrentIndex)
		if isPlaying && i == m.plCur && m.focus == 1 { style = style.Underline(true) }
		if i == m.plCur && m.focus == 1 {
			plContent += m.styles.Cursor.Render(line) + "\n"
		} else {
			if isPlaying { style = m.styles.Neon.Underline(true) }
			plContent += style.Render(line) + "\n"
		}
	}

	// Нижняя часть (Статус и Справка)
	helpText := "TAB: focus | ARROWS: nav | ENTER: action | . , : seek"
	help := m.styles.Help.Render(helpText)
	if m.searchMode { help = m.styles.Neon.Render("SEARCH: " + m.searchInput) }

	// Прогресс-бар во всю ширину справки (53 символа)
	bar := RenderProgressBar(53, m.curPos, m.curDur, lipgloss.Color(m.config.ThemeColor))
	timer := fmt.Sprintf(" %02d:%02d / %02d:%02d", int(m.curPos)/60, int(m.curPos)%60, int(m.curDur)/60, int(m.curDur)%60)
	vol := m.styles.Neon.Render(fmt.Sprintf(" VOL: %d%%", m.state.Volume))

	return lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top, lS.Height(m.height+2).Render(fmContent), rS.Height(m.height+2).Render(plContent)),
		"\n "+bar+timer+vol,
		"\n "+help)
}

func TrimText(s string, w int) string {
	r := []rune(s)
	if len(r) <= w { return s + strings.Repeat(" ", w-len(r)) }
	return string(r[:w-3]) + "..."
}

func RenderProgressBar(width int, cur, total float64, color lipgloss.Color) string {
	char := "━"
	if total <= 0 { return lipgloss.NewStyle().Foreground(lipgloss.Color("#333333")).Render(strings.Repeat(char, width)) }
	filledWidth := int((cur / total) * float64(width))
	if filledWidth > width { filledWidth = width }
	filled := lipgloss.NewStyle().Foreground(color).Render(strings.Repeat(char, filledWidth))
	empty := lipgloss.NewStyle().Foreground(lipgloss.Color("#333333")).Render(strings.Repeat(char, width-filledWidth))
	return filled + empty
}