package preview

import (
	"bufio"
	"fmt"
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

// CacheKey represents a unique key for the cache
type CacheKey string

// cacheEntry holds cached content along with its timestamp
type cacheEntry struct {
	content   string
	timestamp time.Time
}

// ContentCache manages cached diff and preview content
type ContentCache struct {
	mu      sync.RWMutex
	entries map[CacheKey]cacheEntry
	ttl     time.Duration
}

// Get retrieves content for a key or returns false if not found/expired
func (c *ContentCache) Get(key CacheKey) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}

	// Check if the entry is expired
	if time.Since(entry.timestamp) > c.ttl {
		return "", false
	}

	return entry.content, true
}

// Set stores content with the given key
func (c *ContentCache) Set(key CacheKey, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cacheEntry{
		content:   content,
		timestamp: time.Now(),
	}
}

// NewCache creates a content cache with the given TTL
func NewCache(ttl time.Duration) *ContentCache {
	return &ContentCache{
		entries: make(map[CacheKey]cacheEntry),
		ttl:     ttl,
	}
}

// Model represents the preview component
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
	cache            *ContentCache
	prefetching      sync.Map
	currentKey       CacheKey
}

const (
	DebounceTime            = 200 * time.Millisecond
	CacheTTL                = 5 * time.Minute        // Time before cached items expire
	PrefetchDelay           = 50 * time.Millisecond  // Reduced delay between prefetches
	PrefetchDebounce        = 200 * time.Millisecond // Reduced debounce time
	MaxConcurrentPrefetches = 3                      // Allow more concurrent prefetches
	MinScrollInterval       = 100 * time.Millisecond // Reduced minimum time between prefetches
)

var border = lipgloss.NewStyle().Border(lipgloss.NormalBorder())

// Global prefetch semaphore to limit concurrent prefetch operations
var prefetchSemaphore = make(chan struct{}, MaxConcurrentPrefetches)

// Global timestamp to track the last selection change
var lastSelectionChange = time.Now()

type refreshPreviewContentMsg struct {
	Tag int
}

type updatePreviewContentMsg struct {
	Content string
	Key     CacheKey
}

type prefetchContentMsg struct {
	Key CacheKey
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
	// Initialize with the current selection if available
	item := m.context.SelectedItem()
	if item == nil {
		return nil
	}

	key := m.getCacheKey(item)
	m.currentKey = key

	// Immediately load the first view
	return func() tea.Msg {
		content, key, err := m.fetchContent(item)
		if err != nil {
			return nil
		}

		m.cache.Set(key, content)
		return updatePreviewContentMsg{Content: content, Key: key}
	}
}

// getCacheKey creates a unique cache key for the currently selected item
func (m *Model) getCacheKey(item context.SelectedItem) CacheKey {
	switch v := item.(type) {
	case context.SelectedFile:
		return CacheKey(fmt.Sprintf("file:%s:%s", v.ChangeId, v.File))
	case context.SelectedRevision:
		return CacheKey(fmt.Sprintf("revision:%s", v.ChangeId))
	case context.SelectedOperation:
		return CacheKey(fmt.Sprintf("operation:%s", v.OperationId))
	default:
		return ""
	}
}

// fetchContent loads content for given selected item
func (m *Model) fetchContent(item context.SelectedItem) (string, CacheKey, error) {
	var output []byte
	var err error
	key := m.getCacheKey(item)

	switch v := item.(type) {
	case context.SelectedFile:
		output, err = m.context.RunCommandImmediate(jj.Diff(v.ChangeId, v.File, config.Current.Preview.ExtraArgs...))
	case context.SelectedRevision:
		output, err = m.context.RunCommandImmediate(jj.Show(v.ChangeId, config.Current.Preview.ExtraArgs...))
	case context.SelectedOperation:
		output, err = m.context.RunCommandImmediate(jj.OpShow(v.OperationId))
	default:
		return "", "", fmt.Errorf("unknown item type")
	}

	if err != nil {
		return "", key, err
	}

	return string(output), key, nil
}

// prefetchItem loads an item in the background and caches it
func (m *Model) prefetchItem(key CacheKey) tea.Cmd {
	return func() tea.Msg {
		keyCopy := key

		// Skip if we're scrolling too quickly
		now := time.Now()
		if now.Sub(lastSelectionChange) < MinScrollInterval {
			return nil
		}

		// Use a sync.Map to track ongoing prefetches to avoid duplicates
		_, loaded := m.prefetching.LoadOrStore(keyCopy, true)
		if loaded {
			// Already prefetching this item
			return nil
		}

		// Try to acquire a semaphore slot without blocking
		select {
		case prefetchSemaphore <- struct{}{}:
			// Slot acquired, continue with prefetch
		default:
			// No slots available, skip prefetch
			m.prefetching.Delete(keyCopy)
			return nil
		}

		defer func() {
			// Always release the semaphore when done
			<-prefetchSemaphore
			m.prefetching.Delete(keyCopy)
		}()

		item := m.context.SelectedItem()
		// Safety check for nil
		if item == nil {
			return nil
		}

		if m.getCacheKey(item) != keyCopy {
			// Selection changed while we were waiting, abort
			return nil
		}

		content, fetchedKey, err := m.fetchContent(item)
		if err != nil {
			return nil // Silently fail prefetch
		}

		m.cache.Set(fetchedKey, content)
		return updatePreviewContentMsg{Content: content, Key: fetchedKey}
	}
}

// PrefetchMsg is a message used to request prefetching of additional items
type PrefetchMsg struct {
	Items []context.SelectedItem
}

// Prefetch preloads content for a list of items in the background
func (m *Model) Prefetch(items []context.SelectedItem) tea.Cmd {
	return func() tea.Msg {
		// If too many prefetching operations are already happening, just skip
		if len(prefetchSemaphore) >= MaxConcurrentPrefetches {
			return nil
		}

		// If we're scrolling too quickly, throttle prefetching
		now := time.Now()
		if now.Sub(lastSelectionChange) < MinScrollInterval {
			return nil
		}

		// Limit number of prefetch operations per request
		const maxPrefetchItems = 1
		prefetchCount := 0

		for _, item := range items {
			// Skip nil items
			if item == nil {
				continue
			}

			// Stop if we've hit our prefetch limit
			if prefetchCount >= maxPrefetchItems {
				break
			}

			key := m.getCacheKey(item)
			if key == "" || key == m.currentKey {
				continue
			}

			// Skip if already cached
			if _, found := m.cache.Get(key); found {
				continue
			}

			// Skip if already prefetching
			if _, loaded := m.prefetching.LoadOrStore(key, true); loaded {
				continue
			}

			// Increment prefetch count
			prefetchCount++

			// Use a copy of the values for the goroutine to avoid race conditions
			itemCopy := item
			keyCopy := key

			// Try to acquire a semaphore slot without blocking
			select {
			case prefetchSemaphore <- struct{}{}:
				// Slot acquired, continue with prefetch
			default:
				// No slots available, skip prefetch
				m.prefetching.Delete(keyCopy)
				continue
			}

			// Prefetch in background goroutine with a small delay to avoid resource contention
			go func(item context.SelectedItem, key CacheKey) {
				defer func() {
					// Always release the semaphore and mark as no longer prefetching
					<-prefetchSemaphore
					m.prefetching.Delete(key)
				}()

				// Add a delay to stagger prefetch requests
				time.Sleep(PrefetchDelay)

				// Safe guard against nil
				if item == nil {
					return
				}

				content, fetchedKey, err := m.fetchContent(item)
				if err != nil {
					return
				}

				m.cache.Set(fetchedKey, content)
			}(itemCopy, keyCopy)
		}

		return nil
	}
}

func (m *Model) Update(msg tea.Msg) (*Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case updatePreviewContentMsg:
		// Only update display if this is for the current key or we have no content
		if msg.Key == m.currentKey || m.content == "" {
			m.content = msg.Content
			m.contentLineCount = strings.Count(m.content, "\n")
			m.reset()
		}

	case prefetchContentMsg:
		// Schedule background prefetch
		return m, m.prefetchItem(msg.Key)

	case PrefetchMsg:
		// Handle prefetching adjacent items
		return m, m.Prefetch(msg.Items)

	case common.SelectionChangedMsg, common.RefreshMsg:
		// Update the global timestamp to track selection changes
		lastSelectionChange = time.Now()

		m.tag++
		tag := m.tag

		// Get the selected item
		item := m.context.SelectedItem()
		if item == nil {
			return m, nil
		}

		// Create cache key for this selection
		key := m.getCacheKey(item)
		if key == "" {
			return m, nil
		}

		m.currentKey = key

		// Try to get from cache first
		if cachedContent, found := m.cache.Get(key); found {
			// Immediately update with cached content
			m.content = cachedContent
			m.contentLineCount = strings.Count(cachedContent, "\n")
			m.reset()

			// Still schedule a refresh in the background to ensure fresh content
			cmds = append(cmds, tea.Tick(PrefetchDebounce, func(t time.Time) tea.Msg {
				return prefetchContentMsg{Key: key}
			}))
		} else {
			// No cache hit, schedule immediate fetch after debounce
			cmds = append(cmds, tea.Tick(DebounceTime, func(t time.Time) tea.Msg {
				return refreshPreviewContentMsg{Tag: tag}
			}))
		}

		return m, tea.Batch(cmds...)

	case refreshPreviewContentMsg:
		if m.tag == msg.Tag {
			item := m.context.SelectedItem()
			if item == nil {
				return m, nil
			}

			return m, func() tea.Msg {
				content, key, err := m.fetchContent(item)
				if err != nil {
					return nil
				}

				// Update cache
				m.cache.Set(key, content)
				return updatePreviewContentMsg{Content: content, Key: key}
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

	return m, nil
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
	return Model{
		viewRange:   &viewRange{start: 0, end: 0},
		context:     context,
		keyMap:      keyMap,
		help:        help.New(),
		cache:       NewCache(CacheTTL),
		prefetching: sync.Map{},
	}
}
