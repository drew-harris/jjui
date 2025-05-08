package preview

import (
	"bufio"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/idursun/jjui/internal/config"
	"github.com/idursun/jjui/internal/jj"
	"github.com/idursun/jjui/internal/ui/common"
	"github.com/idursun/jjui/internal/ui/context"
)

type viewRange struct {
	start int
	end   int
}

type Model struct {
	tag              int
	viewRange        *viewRange
	help             help.Model
	width            int
	height           int
	content          string
	contentLineCount int
	context          context.AppContext
	keyMap           config.KeyMappings[key.Binding]
	cache            *previewCache
	msgCh            chan tea.Msg // message channel for prefetch updates
}

type previewCache struct {
	mu      sync.RWMutex
	content map[string]string
}

func newPreviewCache() *previewCache {
	return &previewCache{
		content: make(map[string]string),
	}
}

func (c *previewCache) get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	content, ok := c.content[key]
	return content, ok
}

func (c *previewCache) set(key string, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content[key] = content
}

const DebounceTime = 200 * time.Millisecond

var border = lipgloss.NewStyle().Border(lipgloss.NormalBorder())

type refreshPreviewContentMsg struct {
	Tag int
}

type updatePreviewContentMsg struct {
	Content string
}

type prefetchPreviewContentMsg struct {
	Item context.SelectedItem
}

func (m *Model) Width() int {
	return m.width
}

func (m *Model) Height() int {
	return m.height
}

func (m *Model) SetWidth(w int) {
	m.width = w
}

func (m *Model) SetHeight(h int) {
	m.viewRange.end = min(m.viewRange.start+h-3, m.contentLineCount)
	m.height = h
}

func (m *Model) Init() tea.Cmd {
	return nil
}

func (m *Model) getCacheKey(item context.SelectedItem) string {
	switch v := item.(type) {
	case context.SelectedFile:
		return "file:" + v.ChangeId + ":" + v.File
	case context.SelectedRevision:
		return "revision:" + v.ChangeId
	case context.SelectedOperation:
		return "operation:" + v.OperationId
	default:
		return ""
	}
}

func (m *Model) prefetchContent(item context.SelectedItem) tea.Cmd {
	return func() tea.Msg {
		var output []byte
		var err error

		switch v := item.(type) {
		case context.SelectedRevision:
			output, err = m.context.RunCommandImmediate(jj.Show(v.ChangeId))
		case context.SelectedFile:
			output, err = m.context.RunCommandImmediate(jj.Show(v.ChangeId, v.File))
		case context.SelectedOperation:
			output, err = m.context.RunCommandImmediate(jj.OpShow(v.OperationId))
		}

		if err != nil {
			return nil
		}

		content := string(output)
		m.cache.set(m.getCacheKey(item), content)
		return nil
	}
}

func (m *Model) Update(msg tea.Msg) (*Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case updatePreviewContentMsg:
		m.content = msg.Content
		m.contentLineCount = strings.Count(m.content, "\n")
		m.reset()
	case common.SelectionChangedMsg, common.RefreshMsg:
		item := m.context.SelectedItem()
		switch v := item.(type) {
		case context.SelectedRevision:
			if content, ok := m.cache.get(m.getCacheKey(v)); ok {
				m.content = content
				m.contentLineCount = strings.Count(content, "\n")
				m.reset()
			} else {
				cmd = func() tea.Msg {
					output, _ := m.context.RunCommandImmediate(jj.Show(v.ChangeId, config.Current.Preview.ExtraArgs...))
					m.cache.set(m.getCacheKey(v), string(output))
					return updatePreviewContentMsg{Content: string(output)}
				}
			}
			// Aggressively prefetch next 10 revisions in parallel
			if nextItems := m.context.GetNextItems(10); nextItems != nil {
				for _, nitem := range nextItems {
					if rev, isRev := nitem.(context.SelectedRevision); isRev {
						go func(it context.SelectedRevision) {
							output, _ := m.context.RunCommandImmediate(jj.Show(it.ChangeId, config.Current.Preview.ExtraArgs...))
							m.cache.set(m.getCacheKey(it), string(output))
							// If the user is now viewing this revision, update the preview immediately
							if cur, ok := m.context.SelectedItem().(context.SelectedRevision); ok && cur.ChangeId == it.ChangeId {
								select {
								case m.msgCh <- updatePreviewContentMsg{Content: string(output)}:
								default:
								}
							}
						}(rev)
					}
				}
			}
			// Prefetch file-level diffs for the selected revision
			go func(rev context.SelectedRevision) {
				output, _ := m.context.RunCommandImmediate(jj.Show(rev.ChangeId, config.Current.Preview.ExtraArgs...))
				files := extractFilesFromShowOutput(string(output))
				for _, file := range files {
					go func(f string) {
						fileDiff, _ := m.context.RunCommandImmediate(jj.Diff(rev.ChangeId, f, config.Current.Preview.ExtraArgs...))
						m.cache.set("file:"+rev.ChangeId+":"+f, string(fileDiff))
					}(file)
				}
			}(v)
			return m, cmd
		case context.SelectedFile, context.SelectedOperation:
			m.tag++
			tag := m.tag
			return m, tea.Tick(DebounceTime, func(t time.Time) tea.Msg {
				return refreshPreviewContentMsg{Tag: tag}
			})
		}
	case refreshPreviewContentMsg:
		if m.tag == msg.Tag {
			switch item := m.context.SelectedItem().(type) {
			case context.SelectedFile:
				return m, func() tea.Msg {
					output, _ := m.context.RunCommandImmediate(jj.Diff(item.ChangeId, item.File, config.Current.Preview.ExtraArgs...))
					return updatePreviewContentMsg{Content: string(output)}
				}
			case context.SelectedOperation:
				return m, func() tea.Msg {
					output, _ := m.context.RunCommandImmediate(jj.OpShow(item.OperationId))
					return updatePreviewContentMsg{Content: string(output)}
				}
			}
		}
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.keyMap.Preview.ScrollDown):
			if m.viewRange.end < m.contentLineCount {
				m.viewRange.start++
				m.viewRange.end++
			}
		case key.Matches(msg, m.keyMap.Preview.ScrollUp):
			if m.viewRange.start > 0 {
				m.viewRange.start--
				m.viewRange.end--
			}
		case key.Matches(msg, m.keyMap.Preview.HalfPageDown):
			contentHeight := m.contentLineCount
			halfPageSize := m.height / 2
			if halfPageSize+m.viewRange.end > contentHeight {
				halfPageSize = contentHeight - m.viewRange.end
			}

			m.viewRange.start += halfPageSize
			m.viewRange.end += halfPageSize
		case key.Matches(msg, m.keyMap.Preview.HalfPageUp):
			halfPageSize := min(m.height/2, m.viewRange.start)
			m.viewRange.start -= halfPageSize
			m.viewRange.end -= halfPageSize
		}
	}
	return m, cmd
}

func (m *Model) fetchContent(item context.SelectedItem) tea.Cmd {
	return func() tea.Msg {
		var output []byte
		var err error

		switch v := item.(type) {
		case context.SelectedRevision:
			output, err = m.context.RunCommandImmediate(jj.Show(v.ChangeId))
		case context.SelectedFile:
			output, err = m.context.RunCommandImmediate(jj.Show(v.ChangeId, v.File))
		case context.SelectedOperation:
			output, err = m.context.RunCommandImmediate(jj.OpShow(v.OperationId))
		}

		if err != nil {
			return common.CommandCompletedMsg{
				Output: string(output),
				Err:    err,
			}
		}

		content := string(output)
		m.cache.set(m.getCacheKey(item), content)
		return updatePreviewContentMsg{Content: content}
	}
}

func (m *Model) View() string {
	var w strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(m.content))
	current := 0
	for scanner.Scan() {
		line := scanner.Text()
		if current >= m.viewRange.start && current <= m.viewRange.end {
			if current > m.viewRange.start {
				w.WriteString("\n")
			}
			w.WriteString(lipgloss.NewStyle().MaxWidth(m.width - 2).Render(line))
		}
		current++
		if current > m.viewRange.end {
			break
		}
	}
	view := lipgloss.Place(m.width-2, m.height-2, 0, 0, w.String())
	return border.Render(view)
}

func (m *Model) reset() {
	m.viewRange.start, m.viewRange.end = 0, m.height
}

func New(context context.AppContext) Model {
	keyMap := context.KeyMap()
	msgCh := make(chan tea.Msg, 32)
	m := Model{
		viewRange: &viewRange{start: 0, end: 0},
		context:   context,
		keyMap:    keyMap,
		help:      help.New(),
		cache:     newPreviewCache(),
		msgCh:     msgCh,
	}
	// Start a goroutine to listen for prefetch update messages
	go func() {
		for msg := range msgCh {
			_ = msg // discard for now
		}
	}()
	return m
}

// extractFilesFromShowOutput parses the output of jj show and returns a list of files that appear in the diff.
func extractFilesFromShowOutput(output string) []string {
	var files []string
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "diff --git a/") {
			// Example: diff --git a/foo.txt b/foo.txt
			parts := strings.Split(line, " ")
			if len(parts) >= 4 {
				file := strings.TrimPrefix(parts[2], "a/")
				files = append(files, file)
			}
		}
	}
	return files
}
