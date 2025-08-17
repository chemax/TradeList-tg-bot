package tg

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"shoppingbot/internal/list"
	"shoppingbot/internal/store"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Config struct {
	ChatIDs         []int64 // получатели рассылки
	AdminChatID     int64   // чат для бэкапов (0 = выкл)
	BackupEveryDays int     // раз в N дней
	DBPath          string  // путь к sqlite для отправки файла
	SoftMaxRows     int     // «мягкий» предел строк до пагинации (до лимитов Telegram)
}

type Bot struct {
	api   *tgbotapi.BotAPI
	board *list.Board
	store *store.Store
	cfg   Config

	// id «главного» сообщения в каждом чате
	recMu    sync.Mutex
	recMsgID map[int64]int

	// дебаунс рассылки
	debMu sync.Mutex
	debT  *time.Timer
}

// ---- пер-чат настройки отображения ----
type prefs struct {
	Cols int // 2 или 3
	Page int // текущая страница
}

var (
	prefMu      sync.RWMutex
	prefsByChat = map[int64]*prefs{}
)

func getPrefs(chatID int64) *prefs {
	prefMu.RLock()
	p := prefsByChat[chatID]
	prefMu.RUnlock()
	if p != nil {
		return p
	}
	p = &prefs{Cols: 2, Page: 0}
	prefMu.Lock()
	prefsByChat[chatID] = p
	prefMu.Unlock()
	return p
}

// ---------------------------------------

func New(api *tgbotapi.BotAPI, board *list.Board, st *store.Store, cfg Config) *Bot {
	return &Bot{
		api:      api,
		board:    board,
		store:    st,
		cfg:      cfg,
		recMsgID: map[int64]int{},
	}
}

func (b *Bot) Start() {
	// Восстановить отмеченные категории
	if sel, err := b.store.LoadSelected(); err == nil {
		for c, on := range sel {
			if on {
				b.board.Selected[c] = true
			}
		}
	} else {
		log.Printf("load selected: %v", err)
	}

	// Поднять пер-чат настройки (cols/page) для известных получателей
	for _, cid := range b.cfg.ChatIDs {
		if s, _ := b.store.GetSetting(fmt.Sprintf("cols:%d", cid), "2"); s != "" {
			if v, err := strconv.Atoi(s); err == nil && (v == 2 || v == 3) {
				getPrefs(cid).Cols = v
			}
		}
		if s, _ := b.store.GetSetting(fmt.Sprintf("page:%d", cid), "0"); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v >= 0 {
				getPrefs(cid).Page = v
			}
		}
	}

	// Глобальный фильтр (на всю доску)
	if sFilter, err := b.store.GetSetting("filter", "0"); err == nil {
		if v, e := strconv.Atoi(sFilter); e == nil {
			b.board.SetFilter(list.Filter(v))
		}
	}

	// Бэкапы
	go b.backupLoop()

	// Апдейты
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	for up := range updates {
		switch {
		case up.Message != nil:
			b.onMessage(up.Message)
		case up.CallbackQuery != nil:
			b.onCallback(up.CallbackQuery)
		}
	}
}

func (b *Bot) onMessage(m *tgbotapi.Message) {
	switch m.Command() {
	case "start":
		b.sendOrEditForChat(m.Chat.ID)

	case "two":
		p := getPrefs(m.Chat.ID)
		p.Cols = 2
		_ = b.store.SetSetting(fmt.Sprintf("cols:%d", m.Chat.ID), "2")
		b.scheduleBroadcast()

	case "three":
		p := getPrefs(m.Chat.ID)
		p.Cols = 3
		_ = b.store.SetSetting(fmt.Sprintf("cols:%d", m.Chat.ID), "3")
		b.scheduleBroadcast()

	case "filter":
		arg := strings.TrimSpace(strings.TrimPrefix(m.Text, "/filter"))
		b.applyFilterArg(arg)
		b.scheduleBroadcast()

	case "export":
		b.sendExport(m.Chat.ID)

	case "log":
		b.sendJournal(m.Chat.ID, 50)

	default:
		b.sendOrEditForChat(m.Chat.ID)
	}
}

func (b *Bot) onCallback(cb *tgbotapi.CallbackQuery) {
	data := cb.Data
	cid := cb.Message.Chat.ID

	switch {
	case strings.HasPrefix(data, "t:"):
		cat := strings.TrimPrefix(data, "t:")
		from, to := b.board.Toggle(cat)
		_ = b.store.SetSelected(cat, to)
		_ = b.store.AppendJournal(store.JournalEntry{
			TS: time.Now(), ChatID: cid, User: atName(cb.From),
			Category: cat, From: from, To: to,
		})
		b.scheduleBroadcast()

	case strings.HasPrefix(data, "c:"):
		p := getPrefs(cid)
		if strings.HasSuffix(data, "2") {
			p.Cols = 2
			_ = b.store.SetSetting(fmt.Sprintf("cols:%d", cid), "2")
		} else {
			p.Cols = 3
			_ = b.store.SetSetting(fmt.Sprintf("cols:%d", cid), "3")
		}
		// При смене колонок корректнее сбросить страницу
		p.Page = 0
		_ = b.store.SetSetting(fmt.Sprintf("page:%d", cid), "0")
		b.scheduleBroadcast()

	case strings.HasPrefix(data, "f:"):
		arg := strings.TrimPrefix(data, "f:")
		b.applyFilterArg(arg)
		b.scheduleBroadcast()

	case data == "exp:cur":
		b.sendExport(cid)

	case data == "log:show":
		b.sendJournal(cid, 50)

	case data == "p:prev":
		p := getPrefs(cid)
		total := b.totalPages(p.Cols)
		if total > 0 {
			p.Page = (p.Page - 1 + total) % total
			_ = b.store.SetSetting(fmt.Sprintf("page:%d", cid), strconv.Itoa(p.Page))
			b.scheduleBroadcast()
		}

	case data == "p:next":
		p := getPrefs(cid)
		total := b.totalPages(p.Cols)
		if total > 0 {
			p.Page = (p.Page + 1) % total
			_ = b.store.SetSetting(fmt.Sprintf("page:%d", cid), strconv.Itoa(p.Page))
			b.scheduleBroadcast()
		}
	}

	_ = b.answerCallback(cb, "")
}

func (b *Bot) applyFilterArg(arg string) {
	var f list.Filter
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "not", "todo", "need", "n", "не", "н":
		f = list.FilterNotBought
	case "done", "yes", "y", "куплено", "к":
		f = list.FilterBought
	default:
		f = list.FilterAll
	}
	b.board.SetFilter(f)
	_ = b.store.SetSetting("filter", strconv.Itoa(int(f)))
}

func (b *Bot) sendOrEditForChat(chatID int64) {
	text, kb := b.render(chatID)

	b.recMu.Lock()
	msgID := b.recMsgID[chatID]
	b.recMu.Unlock()

	if msgID == 0 {
		msg := tgbotapi.NewMessage(chatID, text)
		msg.ReplyMarkup = kb
		sent, err := b.api.Send(msg)
		if err != nil {
			log.Printf("send to %d: %v", chatID, err)
			return
		}
		b.recMu.Lock()
		b.recMsgID[chatID] = sent.MessageID
		b.recMu.Unlock()
		return
	}

	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, msgID, text, kb)
	if _, err := b.api.Request(edit); err != nil {
		log.Printf("edit %d: %v -> resend", chatID, err)
		msg := tgbotapi.NewMessage(chatID, text)
		msg.ReplyMarkup = kb
		sent, err2 := b.api.Send(msg)
		if err2 != nil {
			log.Printf("resend fail %d: %v", chatID, err2)
			return
		}
		b.recMu.Lock()
		b.recMsgID[chatID] = sent.MessageID
		b.recMu.Unlock()
	}
}

func (b *Bot) scheduleBroadcast() {
	b.debMu.Lock()
	defer b.debMu.Unlock()
	const delay = 250 * time.Millisecond
	if b.debT == nil {
		b.debT = time.NewTimer(delay)
		go func() {
			for range b.debT.C {
				b.broadcast()
				b.debMu.Lock()
				if !b.debT.Stop() {
					select {
					case <-b.debT.C:
					default:
					}
				}
				b.debMu.Unlock()
			}
		}()
	}
	if !b.debT.Stop() {
		select {
		case <-b.debT.C:
		default:
		}
	}
	b.debT.Reset(delay)
}

func (b *Bot) broadcast() {
	for _, chatID := range b.cfg.ChatIDs {
		b.sendOrEditForChat(chatID)
	}
}

func (b *Bot) render(chatID int64) (string, tgbotapi.InlineKeyboardMarkup) {
	_, sel, _, f, _ := b.board.GetStateSnapshot()
	vis := b.board.Visible()

	p := getPrefs(chatID)
	cols, page := p.Cols, p.Page

	// пагинация «по необходимости»
	maxRows := b.cfg.SoftMaxRows
	if maxRows <= 0 {
		maxRows = 12
	}
	perPage := cols * maxRows
	pages := 1
	start, end := 0, len(vis)
	if len(vis) > perPage {
		pages = (len(vis) + perPage - 1) / perPage
		if page >= pages {
			page = 0
			p.Page = 0
			_ = b.store.SetSetting(fmt.Sprintf("page:%d", chatID), "0")
		}
		start = page * perPage
		end = start + perPage
		if end > len(vis) {
			end = len(vis)
		}
		vis = vis[start:end]
	}

	// текст
	title := "🧾 Список покупок"
	filterTxt := map[list.Filter]string{
		list.FilterAll:       "Все",
		list.FilterNotBought: "☐ Не куплено",
		list.FilterBought:    "✅ Куплено",
	}[f]
	summary := b.board.SummaryLine()

	var bld strings.Builder
	fmt.Fprintf(&bld, "%s\n%s\nФильтр: %s\n\n", title, summary, filterTxt)
	if len(vis) == 0 {
		bld.WriteString("— (пусто)")
	} else {
		for _, c := range vis {
			if sel[c] {
				bld.WriteString("• ☐ ")
			} else {
				bld.WriteString("• ✅ ")
			}
			bld.WriteString(c)
			bld.WriteString("\n")
		}
	}
	text := bld.String()

	// клавиатура
	var rows [][]tgbotapi.InlineKeyboardButton
	row := make([]tgbotapi.InlineKeyboardButton, 0, cols)
	for _, c := range vis {
		label := "☐ " + c
		if !sel[c] { // куплено
			label = "✅ " + c
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, "t:"+c))
		if len(row) == cols {
			rows = append(rows, row)
			row = make([]tgbotapi.InlineKeyboardButton, 0, cols)
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}

	// футер: колонки
	ctrl := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("2 колонки", "c:2"),
		tgbotapi.NewInlineKeyboardButtonData("3 колонки", "c:3"),
	}
	rows = append(rows, ctrl)

	// футер: фильтры
	fRow := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("Все", "f:all"),
		tgbotapi.NewInlineKeyboardButtonData("☐ Не куплено", "f:not"),
		tgbotapi.NewInlineKeyboardButtonData("✅ Куплено", "f:done"),
	}
	rows = append(rows, fRow)

	// футер: действия
	act := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("📤 Экспорт", "exp:cur"),
		tgbotapi.NewInlineKeyboardButtonData("📜 Журнал", "log:show"),
	}
	rows = append(rows, act)

	// футер: пагинация (если нужна)
	if pages > 1 {
		pg := []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("◀️ %d/%d", page+1, pages), "p:prev"),
			tgbotapi.NewInlineKeyboardButtonData("▶️", "p:next"),
		}
		rows = append(rows, pg)
	}

	return text, tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (b *Bot) sendExport(chatID int64) {
	lines := b.board.ExportLines() // ВЕСЬ список: [x]/[]
	text := "```text\n" + list.JoinLines(lines) + "\n```"
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("export send: %v", err)
	}
}

func (b *Bot) sendJournal(chatID int64, limit int) {
	entries, err := b.store.LastJournal(limit)
	if err != nil {
		log.Printf("journal: %v", err)
		return
	}
	if len(entries) == 0 {
		_, _ = b.api.Send(tgbotapi.NewMessage(chatID, "Журнал пуст."))
		return
	}
	var sb strings.Builder
	sb.WriteString("```text\n")
	for i := range entries {
		e := entries[i]
		from := "☐"
		if e.From {
			from = "✅" // из купленного — в чекбокс? нет, у нас бинарная логика "в списке"/"куплено"; журнал просто показывает переход
		}
		to := "☐"
		if e.To {
			to = "✅"
		}
		// ts  user  from->to  (chatID)
		sb.WriteString(fmt.Sprintf("%s  %-16s  %s → %s  (%d)\n",
			e.TS.Format("2006-01-02 15:04"), e.User, from, to, e.ChatID))
	}
	sb.WriteString("```")
	msg := tgbotapi.NewMessage(chatID, sb.String())
	msg.ParseMode = "Markdown"
	_, _ = b.api.Send(msg)
}

func (b *Bot) totalPages(cols int) int {
	vis := b.board.Visible()
	maxRows := b.cfg.SoftMaxRows
	if maxRows <= 0 {
		maxRows = 12
	}
	perPage := cols * maxRows
	if len(vis) <= perPage {
		return 0
	}
	return (len(vis) + perPage - 1) / perPage
}

func (b *Bot) answerCallback(cb *tgbotapi.CallbackQuery, text string) error {
	resp := tgbotapi.NewCallback(cb.ID, text)
	_, err := b.api.Request(resp)
	return err
}

func (b *Bot) backupLoop() {
	if b.cfg.AdminChatID == 0 || b.cfg.BackupEveryDays <= 0 {
		return
	}
	interval := time.Duration(b.cfg.BackupEveryDays) * 24 * time.Hour
	t := time.NewTicker(interval)
	for range t.C {
		go b.doBackup()
	}
}

func (b *Bot) doBackup() {
	// 1) Экспорт текущего списка
	lines := b.board.ExportLines()
	txt := "```text\n" + list.JoinLines(lines) + "\n```"
	msg := tgbotapi.NewMessage(b.cfg.AdminChatID, txt)
	msg.ParseMode = "Markdown"
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("backup export: %v", err)
	}
	// 2) Отправка файла БД
	if b.cfg.DBPath != "" {
		doc := tgbotapi.NewDocument(b.cfg.AdminChatID, tgbotapi.FilePath(b.cfg.DBPath))
		doc.Caption = "SQLite backup"
		if _, err := b.api.Send(doc); err != nil {
			log.Printf("backup db file: %v", err)
		}
	}
}

func atName(u *tgbotapi.User) string {
	if u == nil {
		return "кто-то"
	}
	if u.UserName != "" {
		return "@" + u.UserName
	}
	return u.FirstName
}
