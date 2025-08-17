package main

import (
	"log"

	"shoppingbot/internal/config"
	"shoppingbot/internal/list"
	"shoppingbot/internal/store"
	"shoppingbot/internal/tg"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Дефолтные категории на случай пустой БД (однократно засеются при первом запуске)
var DefaultCategories = []string{
	"Овощи", "Фрукты", "Зелень", "Картофель", "Лук/Чеснок",
	"Грибы", "Молочное", "Яйца", "Сыры", "Мясо", "Птица", "Рыба", "Колбаса",
	"Хлеб", "Выпечка", "Крупы", "Макароны", "Масло сливочное", "Масло растительное",
	"Консервы", "Соусы", "Специи", "Чай/Кофе", "Напитки",
	"Сладкое", "Закуски", "Заморозка овощи", "Хозтовары", "Аптека",
	"Кошачий корм", "Кошачий туалет", "Гигиена", "Бытхим",
}

func main() {
	cfg := config.Load()

	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Fatalf("bot init: %v", err)
	}
	api.Debug = false

	st, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("sqlite: %v", err)
	}
	defer st.Close()

	// Если таблица categories пуста — засеять дефолтами
	cats, err := st.ListCategories()
	if err != nil {
		log.Fatalf("load categories: %v", err)
	}
	if len(cats) == 0 {
		for _, c := range DefaultCategories {
			_ = st.AddCategory(c)
		}
		cats, _ = st.ListCategories()
	}

	board := list.NewBoard(cats)

	bot := tg.New(api, board, st, tg.Config{
		ChatIDs:         cfg.ChatIDs,
		AdminChatID:     cfg.AdminChatID,
		BackupEveryDays: cfg.BackupEveryDays,
		DBPath:          cfg.DBPath,
		SoftMaxRows:     12,
	})

	log.Printf("Bot authorized as @%s", api.Self.UserName)
	bot.Start()
}
