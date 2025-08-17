package list

import (
	"fmt"
	"strings"
	"sync"
)

type Filter int

const (
	FilterAll       Filter = iota
	FilterNotBought        // «в списке» (не куплено) — ☐
	FilterBought           // «куплено» — ✅
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

// Полная замена категорий (например, после изменения в БД)
func (b *Board) ReplaceCategories(cats []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// заменить список
	b.Categories = append([]string(nil), cats...)
	// зачистить Selected от несуществующих
	set := make(map[string]struct{}, len(cats))
	for _, c := range cats {
		set[c] = struct{}{}
	}
	for k := range b.Selected {
		if _, ok := set[k]; !ok {
			delete(b.Selected, k)
		}
	}
}

func (b *Board) HasCategory(cat string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, c := range b.Categories {
		if c == cat {
			return true
		}
	}
	return false
}

func (b *Board) Toggle(cat string) (from, to bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// игнор, если категории нет (например, кнопка из старого сообщения)
	found := false
	for _, c := range b.Categories {
		if c == cat {
			found = true
			break
		}
	}
	if !found {
		return false, false
	}

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

// Экспорт ВСЕГО списка: [x] куплено, [] не куплено.
func (b *Board) ExportLines() []string {
	cats, sel, _, _, _ := b.GetStateSnapshot()
	lines := make([]string, 0, len(cats))
	for _, c := range cats {
		if sel[c] {
			lines = append(lines, "[] "+c)
		} else {
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

func JoinLines(lines []string) string { return strings.Join(lines, "\n") }

// SetAllSelected отмечает/снимает все категории на доске.
func (b *Board) SetAllSelected(on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if on {
		for _, c := range b.Categories {
			b.Selected[c] = true
		}
	} else {
		// очистить все отметки (если когда-нибудь понадобится)
		for k := range b.Selected {
			delete(b.Selected, k)
		}
	}
}
