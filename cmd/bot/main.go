package main

import (
	"log"

	"shoppingbot/internal/config"
	"shoppingbot/internal/list"
	"shoppingbot/internal/store"
	"shoppingbot/internal/tg"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Категории можно оставить здесь (или тоже вынести в config при желании).
var Categories = []string{
	"Овощи", "Фрукты", "Зелень", "Картофель", "Лук/Чеснок",
	"Грибы", "Молочное", "Яйца", "Сыры", "Мясо", "Птица", "Рыба",
	"Хлеб", "Выпечка", "Крупы", "Макароны", "Масло/Жиры",
	"Консервы", "Соусы", "Специи", "Чай/Кофе", "Напитки",
	"Сладкое", "Закуски", "Заморозка",
	"Детское", "Зоотовары", "Гигиена", "Бытхим", "Хозтовары", "Аптека",
}

func main() {
	// Загружаем конфиг (токен, chatIDs, путь к БД, бэкапы)
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

	board := list.NewBoard(Categories)

	bot := tg.New(api, board, st, tg.Config{
		ChatIDs:         cfg.ChatIDs,
		AdminChatID:     cfg.AdminChatID,
		BackupEveryDays: cfg.BackupEveryDays,
		DBPath:          cfg.DBPath,
		SoftMaxRows:     12, // мягкий предел строк до автопагинации
	})

	log.Printf("Bot authorized as @%s", api.Self.UserName)
	bot.Start()
}
