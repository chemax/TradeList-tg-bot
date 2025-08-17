package list

import (
	"fmt"
	"strings"
	"sync"
)

type Filter int

const (
	FilterAll       Filter = iota
	FilterNotBought        // «в списке» (не куплено) — отображаем ☐
	FilterBought           // «куплено» — отображаем ✅
)

type Board struct {
	mu         sync.RWMutex
	Categories []string
	Selected   map[string]bool // true = «в списке» (не куплено)
	Filter     Filter
}

func NewBoard(categories []string) *Board {
	return &Board{
		Categories: append([]string(nil), categories...),
		Selected:   map[string]bool{},
		Filter:     FilterAll,
	}
}

func (b *Board) Toggle(cat string) (from, to bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	from = b.Selected[cat]
	if from {
		delete(b.Selected, cat)
		to = false
	} else {
		b.Selected[cat] = true
		to = true
	}
	return
}

func (b *Board) SetFilter(f Filter) {
	b.mu.Lock()
	b.Filter = f
	b.mu.Unlock()
}

func (b *Board) GetStateSnapshot() (cats []string, sel map[string]bool, cols int, f Filter, page int) {
	// cols/page не используются в домене — оставлены для совместимости сигнатуры
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]string(nil), b.Categories...), copyMap(b.Selected), 0, b.Filter, 0
}

func (b *Board) Visible() []string {
	cats, sel, _, f, _ := b.GetStateSnapshot()
	out := make([]string, 0, len(cats))
	switch f {
	case FilterAll:
		for _, c := range cats {
			out = append(out, c)
		}
	case FilterNotBought:
		for _, c := range cats {
			if sel[c] {
				out = append(out, c)
			}
		}
	case FilterBought:
		for _, c := range cats {
			if !sel[c] {
				out = append(out, c)
			}
		}
	}
	return out
}

// ExportLines — экспорт ВСЕГО списка: [x] куплено, [] не куплено.
func (b *Board) ExportLines() []string {
	cats, sel, _, _, _ := b.GetStateSnapshot()
	lines := make([]string, 0, len(cats))
	for _, c := range cats {
		if sel[c] { // в списке (не куплено)
			lines = append(lines, "[] "+c)
		} else { // куплено
			lines = append(lines, "[x] "+c)
		}
	}
	return lines
}

func (b *Board) SummaryLine() string {
	_, sel, _, _, _ := b.GetStateSnapshot()
	total := len(b.Categories)
	notBought := 0
	for _, ok := range sel {
		if ok {
			notBought++
		}
	}
	bought := total - notBought
	return fmt.Sprintf("Всего: %d • Не куплено: %d • Куплено: %d", total, notBought, bought)
}

func copyMap(m map[string]bool) map[string]bool {
	cp := make(map[string]bool, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func JoinLines(lines []string) string {
	return strings.Join(lines, "\n")
}
