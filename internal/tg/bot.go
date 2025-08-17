package tg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	SoftMaxRows     int     // дефолт строк на страницу, если нет пер-чатной настройки
}

type Bot struct {
	api   *tgbotapi.BotAPI
	board *list.Board
	store *store.Store
	cfg   Config

	recMu    sync.Mutex
	recMsgID map[int64]int

	debMu sync.Mutex
	debT  *time.Timer // дебаунс-таймер (через AfterFunc)

	// последняя сигнатура текста+клавиатуры для каждого чата
	lastSig map[int64]string
}

// ---- пер-чат настройки отображения ----
type prefs struct {
	Cols        int  // 2, 3, 4
	Page        int  // текущая страница
	ReorderMode bool // режим упорядочивания
	DelMode     bool // режим быстрого удаления (только AdminChatID)
	DelCand     string
	MenuOpen    bool // подменю открыто
	Rows        int  // максимум строк на страницу (пер-чат)
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
	p = &prefs{
		Cols:        2,
		Page:        0,
		ReorderMode: false,
		DelMode:     false,
		DelCand:     "",
		MenuOpen:    false,
		Rows:        0, // возьмём дефолт потом
	}
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
		lastSig:  map[int64]string{},
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

	// Поднять пер-чат настройки (cols/page/reorder/rows) для известных получателей
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
		if s, _ := b.store.GetSetting(fmt.Sprintf("rows:%d", cid), strconv.Itoa(b.defaultRows())); s != "" {
			if v, err := strconv.Atoi(s); err == nil && v > 0 {
				getPrefs(cid).Rows = v
			}
		}
		// MenuOpen / DelMode не восстанавливаем — это временные UI-состояния
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

	case "backup":
		if m.Chat != nil && b.cfg.AdminChatID != 0 && m.Chat.ID == b.cfg.AdminChatID {
			go b.doBackup()
			_, _ = b.api.Send(tgbotapi.NewMessage(m.Chat.ID, "Запускаю бэкап…"))
		}

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

	// ----- Подменю -----
	case data == "menu:open":
		p.MenuOpen = true
		b.scheduleBroadcast()

	case data == "menu:close":
		p.MenuOpen = false
		b.scheduleBroadcast()

	// ----- Быстрое удаление (только админ-чат) -----
	case data == "delmode:toggle":
		if !isAdminChat {
			break
		}
		p.DelMode = !p.DelMode
		p.DelCand = ""
		p.MenuOpen = false
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
		p.DelCand = ""
		b.scheduleBroadcast()

	// ----- Обычные действия (тумблер/колонки/фильтры/порядок/экспорт/журнал/бэкап/всё/строки) -----
	case strings.HasPrefix(data, "t:"):
		// в режиме удаления игнорируем тумблеры
		if p.DelMode {
			break
		}
		cat := strings.TrimPrefix(data, "t:")
		from, to := b.board.Toggle(cat)
		_ = b.store.SetSelected(cat, to)
		_ = b.store.AppendJournal(store.JournalEntry{
			TS:       time.Now(),
			ChatID:   cid,
			User:     atName(cb.From),
			Category: cat,
			From:     from,
			To:       to,
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

	case strings.HasPrefix(data, "r:"):
		if p.DelMode {
			break
		}
		val := strings.TrimPrefix(data, "r:")

		// Текущее значение (с дефолтом)
		cur := p.Rows
		if cur <= 0 {
			cur = b.defaultRows()
		}

		switch val {
		case "+8":
			cur += 8
		case "-8":
			cur -= 8
		default:
			// совместимость: r:<число>
			if v, err := strconv.Atoi(val); err == nil && v > 0 {
				cur = v
			}
		}
		cur = b.clampRows(cur)

		p.Rows = cur
		p.Page = 0
		_ = b.store.SetSetting(fmt.Sprintf("rows:%d", cid), strconv.Itoa(cur))
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

	case data == "backup:now":
		if isAdminChat {
			go b.doBackup()
		}

	case data == "re:toggle":
		if p.DelMode {
			break
		}
		p.ReorderMode = !p.ReorderMode
		_ = b.store.SetSetting(fmt.Sprintf("reorder:%d", cid), boolTo01(p.ReorderMode))
		p.Page = 0
		p.MenuOpen = false // при входе/выходе из режима порядок — закрыть подменю
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
		total := b.totalPages(p.Cols, p.Rows, p.ReorderMode || p.DelMode)
		if total > 0 {
			p.Page = (p.Page - 1 + total) % total
			_ = b.store.SetSetting(fmt.Sprintf("page:%d", cid), strconv.Itoa(p.Page))
			b.scheduleBroadcast()
		}

	case data == "p:next":
		total := b.totalPages(p.Cols, p.Rows, p.ReorderMode || p.DelMode)
		if total > 0 {
			p.Page = (p.Page + 1) % total
			_ = b.store.SetSetting(fmt.Sprintf("page:%d", cid), strconv.Itoa(p.Page))
			b.scheduleBroadcast()
		}

	case data == "all:not":
		if p.DelMode {
			break
		}
		cats, sel, _, _, _ := b.board.GetStateSnapshot()
		changed := 0
		for _, c := range cats {
			if !sel[c] {
				changed++
			}
		}
		if err := b.store.SetAllSelected(true); err != nil {
			log.Printf("all:not: %v", err)
			break
		}
		b.board.SetAllSelected(true)
		_ = b.store.AppendJournal(store.JournalEntry{
			TS:       time.Now(),
			ChatID:   cid,
			User:     atName(cb.From),
			Category: fmt.Sprintf("ALL → ☐ (+%d)", changed),
			From:     false,
			To:       true,
		})
		b.scheduleBroadcast()
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
	sig := makeSignature(text, kb)

	b.recMu.Lock()
	msgID := b.recMsgID[chatID]
	prevSig := b.lastSig[chatID]
	b.recMu.Unlock()

	// Если уже есть сообщение и контент не менялся — ничего не делаем
	if msgID != 0 && prevSig == sig {
		return
	}

	if msgID == 0 {
		// Первичная отправка
		msg := tgbotapi.NewMessage(chatID, text)
		msg.ReplyMarkup = kb
		sent, err := b.api.Send(msg)
		if err != nil {
			log.Printf("send to %d: %v", chatID, err)
			return
		}
		b.recMu.Lock()
		b.recMsgID[chatID] = sent.MessageID
		b.lastSig[chatID] = sig
		b.recMu.Unlock()
		return
	}

	// Редактирование существующего сообщения
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, msgID, text, kb)
	if _, err := b.api.Request(edit); err != nil {
		if isNotModifiedErr(err) {
			// Ничего не изменилось — просто зафиксируем текущую сигнатуру и выйдем без resend
			b.recMu.Lock()
			b.lastSig[chatID] = sig
			b.recMu.Unlock()
			return
		}
		// Другие ошибки — попробуем переслать заново (как раньше)
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
		b.lastSig[chatID] = sig
		b.recMu.Unlock()
		return
	}

	// Успешно отредактировали — обновим сигнатуру
	b.recMu.Lock()
	b.lastSig[chatID] = sig
	b.recMu.Unlock()
}

// Дебаунс рассылки: безопасно через time.AfterFunc (без Stop на nil)
func (b *Bot) scheduleBroadcast() {
	const delay = 250 * time.Millisecond
	b.debMu.Lock()
	defer b.debMu.Unlock()

	if b.debT == nil {
		b.debT = time.AfterFunc(delay, func() {
			b.broadcast()
			b.debMu.Lock()
			b.debT = nil
			b.debMu.Unlock()
		})
		return
	}
	// Таймер уже есть → просто перезапускаем отсчёт
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
	menu := p.MenuOpen
	isAdminChat := (b.cfg.AdminChatID != 0 && chatID == b.cfg.AdminChatID)

	// Текущее число строк
	curRows := p.Rows
	if curRows <= 0 {
		curRows = b.defaultRows()
	}

	// в режиме удаления — показываем весь список, без фильтра
	if delmode {
		vis = append([]string(nil), cats...)
	}

	// пагинация
	perPage := cols * curRows
	if reorder || delmode {
		perPage = curRows // одна категория = одна строка
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
		modeTxt = " • Режим: 🗑 Удаление"
	}
	if menu {
		modeTxt += " • Меню"
	}
	summary := b.board.SummaryLine()

	var bld strings.Builder
	fmt.Fprintf(&bld, "%s\n%s\nФильтр: %s%s\n\n", title, summary, filterTxt, modeTxt)
	if len(vis) == 0 {
		bld.WriteString("— (пусто)")
	} else {
		for _, c := range vis {
			if !delmode && !reorder {
				if sel[c] {
					bld.WriteString("• ☐ ")
				} else {
					bld.WriteString("• ✅ ")
				}
			} else {
				bld.WriteString("• ")
			}
			bld.WriteString(c)
			bld.WriteString("\n")
		}
	}
	text := bld.String()

	// клавиатура
	var rows [][]tgbotapi.InlineKeyboardButton

	// ----- режим удаления -----
	if delmode {
		for _, c := range vis {
			if p.DelCand == c {
				btn := tgbotapi.NewInlineKeyboardButtonData("❗Подтвердить: "+c, "del:confirm:"+c)
				rows = append(rows, []tgbotapi.InlineKeyboardButton{btn})
			} else {
				btn := tgbotapi.NewInlineKeyboardButtonData("✖ "+c, "del:arm:"+c)
				rows = append(rows, []tgbotapi.InlineKeyboardButton{btn})
			}
		}
		// назад
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("↩️ Назад", "delmode:toggle"),
		})
		// пагинация
		if pages > 1 {
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("◀️ %d/%d", page+1, pages), "p:prev"),
				tgbotapi.NewInlineKeyboardButtonData("▶️", "p:next"),
			})
		}
		return text, tgbotapi.NewInlineKeyboardMarkup(rows...)
	}

	// ----- режим порядка -----
	if reorder {
		for _, c := range vis {
			up := tgbotapi.NewInlineKeyboardButtonData("⬆️", "up:"+c)
			center := tgbotapi.NewInlineKeyboardButtonData("≡ "+c, "top:"+c) // клик по названию -> вверх
			down := tgbotapi.NewInlineKeyboardButtonData("⬇️", "down:"+c)
			rows = append(rows, []tgbotapi.InlineKeyboardButton{up, center, down})
		}
		// кнопка «Готово»
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("✅ Готово", "re:toggle"),
		})
		// пагинация
		if pages > 1 {
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("◀️ %d/%d", page+1, pages), "p:prev"),
				tgbotapi.NewInlineKeyboardButtonData("▶️", "p:next"),
			})
		}
		return text, tgbotapi.NewInlineKeyboardMarkup(rows...)
	}

	// ----- обычный режим (категории + подменю + пагинация) -----
	// 1) категории (тумблеры)
	row := make([]tgbotapi.InlineKeyboardButton, 0, cols)
	for _, c := range vis {
		label := "☐ " + c
		if !sel[c] { // false = куплено => ✅
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

	// 2) подменю
	if !menu {
		// только кнопка «⋯ Меню»
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("⋯ Меню", "menu:open"),
		})
	} else {
		// заголовок подменю: Назад
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "menu:close"),
		})

		// Фильтры
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("Все", "f:all"),
			tgbotapi.NewInlineKeyboardButtonData("☐ Не куплено", "f:not"),
			tgbotapi.NewInlineKeyboardButtonData("✅ Куплено", "f:done"),
		})

		// Колонки
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("2 колонки", "c:2"),
			tgbotapi.NewInlineKeyboardButtonData("3 колонки", "c:3"),
			tgbotapi.NewInlineKeyboardButtonData("4 колонки", "c:4"),
		})

		// Строки: −8 / текущие / +8
		dec := tgbotapi.NewInlineKeyboardButtonData("−8", "r:-8")
		mid := tgbotapi.InlineKeyboardButton{Text: fmt.Sprintf("Строк %d", curRows), CallbackData: strPtr("noop")}
		inc := tgbotapi.NewInlineKeyboardButtonData("+8", "r:+8")
		rows = append(rows, []tgbotapi.InlineKeyboardButton{dec, mid, inc})

		// Действия: добавить / всё некупленным
		addBtn := tgbotapi.InlineKeyboardButton{Text: "➕ Категория"}
		empty := ""
		addBtn.SwitchInlineQueryCurrentChat = &empty
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			addBtn,
			tgbotapi.NewInlineKeyboardButtonData("☐ Всё", "all:not"),
		})

		// Экспорт / Журнал
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📤 Экспорт", "exp:cur"),
			tgbotapi.NewInlineKeyboardButtonData("📜 Журнал", "log:show"),
		})

		// Порядок (включение режима)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("↕️ Порядок", "re:toggle"),
		})

		// Админ-блок
		if isAdminChat {
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData("🗑 Удалить", "delmode:toggle"),
				tgbotapi.NewInlineKeyboardButtonData("💾 Бэкап", "backup:now"),
			})
		}
	}

	// 3) пагинация (всегда внизу, если нужна)
	if pages > 1 {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("◀️ %d/%d", page+1, pages), "p:prev"),
			tgbotapi.NewInlineKeyboardButtonData("▶️", "p:next"),
		})
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
		from := "✅"
		if e.From {
			from = "☐"
		}
		to := "✅"
		if e.To {
			to = "☐"
		}
		sb.WriteString(fmt.Sprintf("%s  %s  [%s]  %s → %s  (%d)\n",
			e.TS.Format("2006-01-02 15:04"), e.User, e.Category, from, to, e.ChatID))
	}
	sb.WriteString("```")
	msg := tgbotapi.NewMessage(chatID, sb.String())
	msg.ParseMode = "Markdown"
	_, _ = b.api.Send(msg)
}

func (b *Bot) totalPages(cols int, rows int, singleRow bool) int {
	vis := b.board.Visible()
	maxRows := rows
	if maxRows <= 0 {
		maxRows = b.defaultRows()
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

func boolTo01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (b *Bot) defaultRows() int {
	if b.cfg.SoftMaxRows > 0 {
		return b.cfg.SoftMaxRows
	}
	return 12
}

func (b *Bot) clampRows(n int) int {
	if n < 4 {
		return 4
	}
	if n > 48 {
		return 48
	}
	return n
}

// makeSignature — стабильная сигнатура текста и inline-клавиатуры
func makeSignature(text string, kb tgbotapi.InlineKeyboardMarkup) string {
	bin, _ := json.Marshal(kb)
	h := sha256.Sum256(append([]byte(text), bin...))
	return hex.EncodeToString(h[:])
}

// isNotModifiedErr — true, если это "message is not modified"
func isNotModifiedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "message is not modified")
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
