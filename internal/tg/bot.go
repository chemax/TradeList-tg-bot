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
	AdminChatID     int64   // чат для бэкапов и админ-режимов (0 = выкл)
	BackupEveryDays int     // раз в N дней
	DBPath          string  // путь к sqlite для отправки файла
	SoftMaxRows     int     // «мягкий» предел строк до пагинации (до лимитов Telegram)
}

type Bot struct {
	api   *tgbotapi.BotAPI
	board *list.Board
	store *store.Store
	cfg   Config

	recMu    sync.Mutex
	recMsgID map[int64]int

	debMu sync.Mutex
	debT  *time.Timer
}

// ---- пер-чат настройки отображения ----
type prefs struct {
	Cols        int  // 2, 3, 4
	Page        int  // текущая страница
	ReorderMode bool // режим упорядочивания
	DelMode     bool // режим быстрого удаления (только для AdminChatID)
	DelCand     string
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
	p = &prefs{Cols: 2, Page: 0, ReorderMode: false, DelMode: false, DelCand: ""}
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

	// Поднять категории из БД (порядок!)
	if cats, err := b.store.ListCategories(); err == nil && len(cats) > 0 {
		b.board.ReplaceCategories(cats)
	}

	// Поднять пер-чат настройки (cols/page/reorder) для известных получателей
	for _, cid := range b.cfg.ChatIDs {
		if s, _ := b.store.GetSetting(fmt.Sprintf("cols:%d", cid), "2"); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v >= 2 && v <= 4 {
				getPrefs(cid).Cols = v
			}
		}
		if s, _ := b.store.GetSetting(fmt.Sprintf("page:%d", cid), "0"); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v >= 0 {
				getPrefs(cid).Page = v
			}
		}
		if s, _ := b.store.GetSetting(fmt.Sprintf("reorder:%d", cid), "0"); s != "" {
			getPrefs(cid).ReorderMode = s == "1"
		}
		// DelMode не восстанавливаем — это временный режим, только из админ-чата по кнопке
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
		case up.InlineQuery != nil:
			b.onInlineQuery(up.InlineQuery)
		}
	}
}

func (b *Bot) onMessage(m *tgbotapi.Message) {
	switch m.Command() {
	case "start":
		b.sendOrEditForChat(m.Chat.ID)
	case "backup":
		if m.Chat != nil && b.cfg.AdminChatID != 0 && m.Chat.ID == b.cfg.AdminChatID {
			go b.doBackup()
			_, _ = b.api.Send(tgbotapi.NewMessage(m.Chat.ID, "Запускаю бэкап…"))
		}
	case "allnot":
		pr := getPrefs(m.Chat.ID)
		if pr.DelMode {
			_, _ = b.api.Send(tgbotapi.NewMessage(m.Chat.ID, "Сейчас включён режим удаления. Выйдите из него, чтобы отметить всё."))
			return
		}

		cats, sel, _, _, _ := b.board.GetStateSnapshot()
		changed := 0
		for _, c := range cats {
			if !sel[c] {
				changed++
			}
		}

		if err := b.store.SetAllSelected(true); err == nil {
			b.board.SetAllSelected(true)

			// агрегированная запись в журнал
			_ = b.store.AppendJournal(store.JournalEntry{
				TS:       time.Now(),
				ChatID:   m.Chat.ID,
				User:     atName(m.From),
				Category: fmt.Sprintf("ALL → ☐ (+%d)", changed),
				From:     false,
				To:       true,
			})

			b.scheduleBroadcast()
			_, _ = b.api.Send(tgbotapi.NewMessage(m.Chat.ID, "Отмечено как не куплено: всё."))
		} else {
			_, _ = b.api.Send(tgbotapi.NewMessage(m.Chat.ID, "Не удалось отметить всё: "+err.Error()))
		}
	case "two":
		p := getPrefs(m.Chat.ID)
		p.Cols, p.Page = 2, 0
		_ = b.store.SetSetting(fmt.Sprintf("cols:%d", m.Chat.ID), "2")
		_ = b.store.SetSetting(fmt.Sprintf("page:%d", m.Chat.ID), "0")
		b.scheduleBroadcast()

	case "three":
		p := getPrefs(m.Chat.ID)
		p.Cols, p.Page = 3, 0
		_ = b.store.SetSetting(fmt.Sprintf("cols:%d", m.Chat.ID), "3")
		_ = b.store.SetSetting(fmt.Sprintf("page:%d", m.Chat.ID), "0")
		b.scheduleBroadcast()

	case "four":
		p := getPrefs(m.Chat.ID)
		p.Cols, p.Page = 4, 0
		_ = b.store.SetSetting(fmt.Sprintf("cols:%d", m.Chat.ID), "4")
		_ = b.store.SetSetting(fmt.Sprintf("page:%d", m.Chat.ID), "0")
		b.scheduleBroadcast()

	case "filter":
		arg := strings.TrimSpace(strings.TrimPrefix(m.Text, "/filter"))
		b.applyFilterArg(arg)
		b.scheduleBroadcast()

	case "export":
		b.sendExport(m.Chat.ID)

	case "log":
		b.sendJournal(m.Chat.ID, 50)

	case "addcat":
		args := strings.TrimSpace(strings.TrimPrefix(m.Text, "/addcat"))
		added := b.addCategoriesFromText(args)
		reply := "Нечего добавлять."
		if added > 0 {
			reply = fmt.Sprintf("Добавлено категорий: %d", added)
		}
		_, _ = b.api.Send(tgbotapi.NewMessage(m.Chat.ID, reply))

	case "delcat":
		args := strings.TrimSpace(strings.TrimPrefix(m.Text, "/delcat"))
		deleted := b.deleteCategoriesFromText(args)
		reply := "Нечего удалять."
		if deleted > 0 {
			reply = fmt.Sprintf("Удалено категорий: %d", deleted)
		}
		_, _ = b.api.Send(tgbotapi.NewMessage(m.Chat.ID, reply))

	default:
		b.sendOrEditForChat(m.Chat.ID)
	}
}

func (b *Bot) onCallback(cb *tgbotapi.CallbackQuery) {
	data := cb.Data
	cid := cb.Message.Chat.ID
	isAdminChat := (b.cfg.AdminChatID != 0 && cid == b.cfg.AdminChatID)
	p := getPrefs(cid)

	switch {
	case data == "noop":
	// ничего, просто закрыть "часики"
	case data == "all:not":
		if p.DelMode {
			break
		} // в режиме удаления игнорим

		// посчитаем, сколько реально изменится
		cats, sel, _, _, _ := b.board.GetStateSnapshot()
		changed := 0
		for _, c := range cats {
			if !sel[c] { // было куплено, станет "в списке" (не куплено)
				changed++
			}
		}

		if err := b.store.SetAllSelected(true); err != nil {
			log.Printf("all:not: %v", err)
			break
		}
		// синхронизируем память и разошлём
		b.board.SetAllSelected(true)

		// агрегированная запись в журнал — одной строкой
		_ = b.store.AppendJournal(store.JournalEntry{
			TS:       time.Now(),
			ChatID:   cid,
			User:     atName(cb.From),
			Category: fmt.Sprintf("ALL → ☐ (+%d)", changed),
			From:     false, // формально "смешанное", но отображаем как сводную стрелку
			To:       true,
		})

		b.scheduleBroadcast()
	case data == "backup:now":
		if isAdminChat {
			go b.doBackup() // не блокируем UI, шлём экспорт и файл в админ-чат
		}
	// ----- Быстрое удаление (только админ-чат) -----
	case data == "delmode:toggle":
		if !isAdminChat {
			break
		}
		p.DelMode = !p.DelMode
		p.DelCand = ""
		b.scheduleBroadcast()

	case data == "del:clear":
		if !isAdminChat || !p.DelMode {
			break
		}
		p.DelCand = ""
		b.scheduleBroadcast()

	case strings.HasPrefix(data, "del:arm:"):
		if !isAdminChat || !p.DelMode {
			break
		}
		cat := strings.TrimPrefix(data, "del:arm:")
		// армим выбранную категорию
		p.DelCand = cat
		b.scheduleBroadcast()

	case strings.HasPrefix(data, "del:confirm:"):
		if !isAdminChat || !p.DelMode {
			break
		}
		cat := strings.TrimPrefix(data, "del:confirm:")
		if err := b.store.DeleteCategory(cat); err != nil {
			log.Printf("quick delete: %v", err)
		}
		if cats, err := b.store.ListCategories(); err == nil {
			b.board.ReplaceCategories(cats)
		}
		// сбрасываем выбор, остаёмся в режиме удаления
		p.DelCand = ""
		b.scheduleBroadcast()

	// ----- Обычный тумблер/колонки/фильтры/порядок -----
	case strings.HasPrefix(data, "t:"):
		// если включён режим удаления — игнорируем тумблеры
		if p.DelMode {
			break
		}
		cat := strings.TrimPrefix(data, "t:")
		from, to := b.board.Toggle(cat)
		_ = b.store.SetSelected(cat, to)
		_ = b.store.AppendJournal(store.JournalEntry{
			TS: time.Now(), ChatID: cid, User: atName(cb.From),
			Category: cat, From: from, To: to,
		})
		b.scheduleBroadcast()

	case strings.HasPrefix(data, "c:"):
		if p.DelMode {
			break
		}
		val := strings.TrimPrefix(data, "c:")
		v, _ := strconv.Atoi(val)
		if v < 2 || v > 4 {
			v = 2
		}
		p.Cols, p.Page = v, 0
		_ = b.store.SetSetting(fmt.Sprintf("cols:%d", cid), strconv.Itoa(v))
		_ = b.store.SetSetting(fmt.Sprintf("page:%d", cid), "0")
		b.scheduleBroadcast()

	case strings.HasPrefix(data, "f:"):
		if p.DelMode {
			break
		}
		arg := strings.TrimPrefix(data, "f:")
		b.applyFilterArg(arg)
		b.scheduleBroadcast()

	case data == "exp:cur":
		if p.DelMode {
			break
		}
		b.sendExport(cid)

	case data == "log:show":
		if p.DelMode {
			break
		}
		b.sendJournal(cid, 50)

	case data == "re:toggle":
		if p.DelMode {
			break
		}
		p.ReorderMode = !p.ReorderMode
		_ = b.store.SetSetting(fmt.Sprintf("reorder:%d", cid), boolTo01(p.ReorderMode))
		p.Page = 0
		_ = b.store.SetSetting(fmt.Sprintf("page:%d", cid), "0")
		b.scheduleBroadcast()

	case strings.HasPrefix(data, "up:"):
		if p.DelMode {
			break
		}
		cat := strings.TrimPrefix(data, "up:")
		if err := b.store.MoveCategoryUp(cat); err != nil {
			log.Printf("move up: %v", err)
		}
		if cats, err := b.store.ListCategories(); err == nil {
			b.board.ReplaceCategories(cats)
		}
		b.scheduleBroadcast()

	case strings.HasPrefix(data, "down:"):
		if p.DelMode {
			break
		}
		cat := strings.TrimPrefix(data, "down:")
		if err := b.store.MoveCategoryDown(cat); err != nil {
			log.Printf("move down: %v", err)
		}
		if cats, err := b.store.ListCategories(); err == nil {
			b.board.ReplaceCategories(cats)
		}
		b.scheduleBroadcast()

	case strings.HasPrefix(data, "top:"):
		if p.DelMode {
			break
		}
		cat := strings.TrimPrefix(data, "top:")
		if err := b.store.MoveCategoryTop(cat); err != nil {
			log.Printf("move top: %v", err)
		}
		if cats, err := b.store.ListCategories(); err == nil {
			b.board.ReplaceCategories(cats)
		}
		b.scheduleBroadcast()

	case data == "p:prev":
		total := b.totalPages(p.Cols, p.ReorderMode || p.DelMode)
		if total > 0 {
			p.Page = (p.Page - 1 + total) % total
			_ = b.store.SetSetting(fmt.Sprintf("page:%d", cid), strconv.Itoa(p.Page))
			b.scheduleBroadcast()
		}

	case data == "p:next":
		total := b.totalPages(p.Cols, p.ReorderMode || p.DelMode)
		if total > 0 {
			p.Page = (p.Page + 1) % total
			_ = b.store.SetSetting(fmt.Sprintf("page:%d", cid), strconv.Itoa(p.Page))
			b.scheduleBroadcast()
		}
	}

	_ = b.answerCallback(cb, "")
}

func (b *Bot) onInlineQuery(q *tgbotapi.InlineQuery) {
	query := strings.TrimSpace(q.Query)
	if query == "" {
		cfg := tgbotapi.InlineConfig{
			InlineQueryID: q.ID,
			IsPersonal:    true,
			CacheTime:     0,
			Results:       []interface{}{},
		}
		_, _ = b.api.Request(cfg)
		return
	}
	title := fmt.Sprintf("Добавить категорию «%s»", query)
	res := tgbotapi.NewInlineQueryResultArticle("add-"+q.ID, title, "/addcat "+query)
	res.Description = "Отправит команду /addcat в этот чат"

	cfg := tgbotapi.InlineConfig{
		InlineQueryID: q.ID,
		IsPersonal:    true,
		CacheTime:     0,
		Results:       []interface{}{res},
	}
	_, _ = b.api.Request(cfg)
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
	cats, sel, _, f, _ := b.board.GetStateSnapshot()
	vis := b.board.Visible()

	p := getPrefs(chatID)
	cols, page := p.Cols, p.Page
	reorder := p.ReorderMode
	delmode := p.DelMode
	isAdminChat := (b.cfg.AdminChatID != 0 && chatID == b.cfg.AdminChatID)

	// режим удаления: список всегда полный, без фильтрации (чтобы ничего не спрятать)
	if delmode {
		vis = append([]string(nil), cats...)
	}

	// пагинация «по необходимости»
	maxRows := b.cfg.SoftMaxRows
	if maxRows <= 0 {
		maxRows = 12
	}

	perPage := cols * maxRows
	if reorder || delmode { // одна категория = одна строка
		perPage = maxRows
	}

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
	modeTxt := ""
	if reorder {
		modeTxt = " • Режим: ↕️ Порядок"
	}
	if delmode {
		modeTxt = " • Режим: 🗑 Удаление (только админ)"
	}
	summary := b.board.SummaryLine()

	var bld strings.Builder
	fmt.Fprintf(&bld, "%s\n%s\nФильтр: %s%s\n\n", title, summary, filterTxt, modeTxt)
	if len(vis) == 0 {
		bld.WriteString("— (пусто)")
	} else if !delmode {
		for _, c := range vis {
			if sel[c] {
				bld.WriteString("• ☐ ")
			} else {
				bld.WriteString("• ✅ ")
			}
			bld.WriteString(c)
			bld.WriteString("\n")
		}
	} else {
		for _, c := range vis {
			bld.WriteString("• ")
			bld.WriteString(c)
			bld.WriteString("\n")
		}
	}
	text := bld.String()

	// клавиатура
	var rows [][]tgbotapi.InlineKeyboardButton

	if delmode {
		// режим быстрое удаление
		for _, c := range vis {
			if p.DelCand == c {
				btn := tgbotapi.NewInlineKeyboardButtonData("❗Подтвердить: "+c, "del:confirm:"+c)
				rows = append(rows, []tgbotapi.InlineKeyboardButton{btn})
			} else {
				btn := tgbotapi.NewInlineKeyboardButtonData("✖ "+c, "del:arm:"+c)
				rows = append(rows, []tgbotapi.InlineKeyboardButton{btn})
			}
		}
		// футер админ-режима
		foot := []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("↩️ Назад", "delmode:toggle"),
		}
		if p.DelCand != "" {
			foot = append(foot, tgbotapi.NewInlineKeyboardButtonData("Отмена выбора", "del:clear"))
		}
		rows = append(rows, foot)

		// пагинация (если нужна)
		pages := b.totalPages(cols, true)
		if pages > 1 {
			pg := []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("◀️ %d/%d", page+1, pages), "p:prev"),
				tgbotapi.NewInlineKeyboardButtonData("▶️", "p:next"),
			}
			rows = append(rows, pg)
		}

		return text, tgbotapi.NewInlineKeyboardMarkup(rows...)
	}

	if !reorder {
		// обычный режим: тумблеры (2/3/4 колонки)
		row := make([]tgbotapi.InlineKeyboardButton, 0, cols)
		for _, c := range vis {
			label := "☐ " + c
			if !sel[c] {
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
	} else {
		// режим порядка: ⬆️  «≡ Cat»  ⬇️
		for _, c := range vis {
			up := tgbotapi.NewInlineKeyboardButtonData("⬆️", "up:"+c)
			center := tgbotapi.NewInlineKeyboardButtonData("≡ "+c, "top:"+c) // клик по названию -> вверх
			down := tgbotapi.NewInlineKeyboardButtonData("⬇️", "down:"+c)
			rows = append(rows, []tgbotapi.InlineKeyboardButton{up, center, down})
		}
	}

	// футер: колонки (2/3/4) — показываем только в обычном режиме
	if !reorder {
		ctrl := []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("2 колонки", "c:2"),
			tgbotapi.NewInlineKeyboardButtonData("3 колонки", "c:3"),
			tgbotapi.NewInlineKeyboardButtonData("4 колонки", "c:4"),
		}
		rows = append(rows, ctrl)
	}

	// футер: фильтры
	fRow := []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("Все", "f:all"),
		tgbotapi.NewInlineKeyboardButtonData("☐ Не куплено", "f:not"),
		tgbotapi.NewInlineKeyboardButtonData("✅ Куплено", "f:done"),
	}
	rows = append(rows, fRow)

	// футер: действия
	addBtn := tgbotapi.InlineKeyboardButton{Text: "➕ Категория"}
	empty := ""
	addBtn.SwitchInlineQueryCurrentChat = &empty

	act := []tgbotapi.InlineKeyboardButton{addBtn}
	// Кнопка экспорта и журнала всегда доступны
	act = append(act,
		tgbotapi.NewInlineKeyboardButtonData("☐ Всё", "all:not"),
		tgbotapi.NewInlineKeyboardButtonData("📤 Экспорт", "exp:cur"),
		tgbotapi.NewInlineKeyboardButtonData("📜 Журнал", "log:show"),
	)
	// Админская кнопка «🗑 Удалить» — только в админ-чате
	if isAdminChat {
		act = append(act, tgbotapi.NewInlineKeyboardButtonData("🗑 Удалить", "delmode:toggle"))
		act = append(act, tgbotapi.NewInlineKeyboardButtonData("💾 Бэкап", "backup:now"))
	}
	rows = append(rows, act)

	// футер: режим порядка
	reBtnText := "↕️ Порядок"
	if reorder {
		reBtnText = "✅ Готово"
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData(reBtnText, "re:toggle"),
	})

	// футер: пагинация (если нужна)
	pages = b.totalPages(cols, reorder)
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
		// true = в списке (не куплено) => ☐ ; false = куплено => ✅
		from := "✅"
		if e.From {
			from = "☐"
		}
		to := "✅"
		if e.To {
			to = "☐"
		}

		// Добавили [Категория]
		sb.WriteString(fmt.Sprintf("%s  %s  [%s]  %s → %s  \n",
			e.TS.Format("2006-01-02 15:04"),
			e.User,
			e.Category,
			from, to,
		))
	}
	sb.WriteString("```")

	msg := tgbotapi.NewMessage(chatID, sb.String())
	msg.ParseMode = "Markdown"
	_, _ = b.api.Send(msg)
}

func (b *Bot) totalPages(cols int, singleRow bool) int {
	vis := b.board.Visible()
	maxRows := b.cfg.SoftMaxRows
	if maxRows <= 0 {
		maxRows = 12
	}
	perPage := cols * maxRows
	if singleRow {
		perPage = maxRows
	}
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

func strPtr(s string) *string { return &s }

// --- вспомогательные для add/del категорий ---
func (b *Bot) addCategoriesFromText(s string) int {
	list := splitCats(s)
	if len(list) == 0 {
		return 0
	}
	cnt := 0
	for _, c := range list {
		if c == "" {
			continue
		}
		if err := b.store.AddCategory(c); err == nil {
			cnt++
		}
	}
	if cats, err := b.store.ListCategories(); err == nil {
		b.board.ReplaceCategories(cats)
		b.scheduleBroadcast()
	}
	return cnt
}

func (b *Bot) deleteCategoriesFromText(s string) int {
	list := splitCats(s)
	if len(list) == 0 {
		return 0
	}
	cnt := 0
	for _, c := range list {
		if c == "" {
			continue
		}
		if err := b.store.DeleteCategory(c); err == nil {
			cnt++
		}
	}
	if cats, err := b.store.ListCategories(); err == nil {
		b.board.ReplaceCategories(cats)
		b.scheduleBroadcast()
	}
	return cnt
}

func splitCats(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	repl := strings.NewReplacer(";", ",", "|", ",", "\n", ",", "\r", ",")
	s = repl.Replace(s)
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func boolTo01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
