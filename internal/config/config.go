package config

import (
	"os"
	"strconv"
	"strings"
)

// ===== ДЕФОЛТЫ (можно править под себя) =====

const DefaultBotToken = "PASTE_YOUR_BOT_TOKEN_HERE"

var DefaultChatIDs = []int64{
	// 123456789,
	// -1001234567890,
}

const DefaultDBPath = "./shopping.db"

// Куда слать бэкапы раз в N дней (0 = выкл)
const DefaultAdminChatID int64 = 0
const DefaultBackupEveryDays = 7

// ===== Итоговая структура =====

type Config struct {
	BotToken        string
	ChatIDs         []int64
	DBPath          string
	AdminChatID     int64
	BackupEveryDays int
}

// Load собирает конфиг из окружения и/или дефолтов.
//
// Поддерживаемые переменные окружения:
//
//	BOT_TOKEN="123:ABC"
//	CHAT_IDS="123456789,-1001234567890"
//	DB_PATH="./shopping.db"
//	ADMIN_CHAT_ID="-1001234567890"
//	BACKUP_EVERY_DAYS="7"
func Load() Config {
	token := getEnv("BOT_TOKEN", DefaultBotToken)
	ids := parseChatIDs(getEnv("CHAT_IDS", ""))
	if len(ids) == 0 {
		ids = append([]int64(nil), DefaultChatIDs...)
	}
	dbPath := getEnv("DB_PATH", DefaultDBPath)
	admin := parseInt64(getEnv("ADMIN_CHAT_ID", ""), DefaultAdminChatID)
	bkup := parseInt(getEnv("BACKUP_EVERY_DAYS", ""), DefaultBackupEveryDays)

	return Config{
		BotToken:        token,
		ChatIDs:         ids,
		DBPath:          dbPath,
		AdminChatID:     admin,
		BackupEveryDays: bkup,
	}
}

// ===== Вспомогательные функции =====

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func parseChatIDs(s string) []int64 {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if v, err := strconv.ParseInt(p, 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func parseInt(s string, def int) int {
	if strings.TrimSpace(s) == "" {
		return def
	}
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}

func parseInt64(s string, def int64) int64 {
	if strings.TrimSpace(s) == "" {
		return def
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		return v
	}
	return def
}
