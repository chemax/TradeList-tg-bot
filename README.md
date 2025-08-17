# README — ShoppingBot (Go + Telegram)

Минималистичный Telegram-бот «механический список покупок»:

- бинарные тумблеры **☐ / ✅** (не куплено / куплено);
- фильтры: **Все / ☐ Не куплено / ✅ Куплено**;
- **2/3/4 колонки** — настраиваются **пер-чат**;
- автопагинация «только если нужно»;
- управление категориями из чата: `/addcat`, `/delcat`, **инлайн-добавление**;
- **порядок категорий**: режим «↕️ Порядок» (⬆️/⬇️/клик по названию = переместить в топ), хранится в SQLite (общий для всех чатов);
- **журнал** изменений с названием категории;
- экспорт списка в ```…``` с пометками `[x]/[]`;
- бэкап списка и файла БД в админ-чат раз в `N` дней;
- **админский режим «быстрое удаление»** (только в админ-чате) с подтверждением.

## Архитектура

```
shoppingbot/
├─ cmd/bot/main.go                # старт и конфиг
├─ internal/
│  ├─ config/config.go            # BOT_TOKEN, CHAT_IDS, DB_PATH, ADMIN_CHAT_ID, BACKUP_EVERY_DAYS
│  ├─ list/list.go                # доменная логика (категории/фильтр/экспорт)
│  ├─ store/sqlite.go             # SQLite: категории/selected/настройки/журнал + перемещения
│  ├─ store/sqlite_driver_cgo.go  # //go:build cgo   -> github.com/mattn/go-sqlite3
│  ├─ store/sqlite_driver_pure.go # //go:build !cgo  -> modernc.org/sqlite (pure Go)
│  └─ tg/tg.go                    # Telegram-слой, клавиатуры, режимы, inline, админ-удаление
```

## Требования

- Go **1.22+**
- Созданный бот в Telegram (@BotFather), **включён inline-режим** (`/setinline` → Enable).
- Linux (для запуска через systemd).

## Сборка

### Вариант A — без CGO (проще, кросс-платформенно)

Драйвер SQLite: `modernc.org/sqlite` (pure Go).

```bash
export CGO_ENABLED=0
go mod tidy
go build -o shoppingbot ./cmd/bot
```

### Вариант B — с CGO (чуть быстрее на больших БД)

Драйвер: `github.com/mattn/go-sqlite3`.

```bash
# Установите компилятор C:
# Debian/Ubuntu: sudo apt-get install -y build-essential
# Alpine:        sudo apk add --no-cache build-base
export CGO_ENABLED=1
go build -o shoppingbot ./cmd/bot
```

> В проекте уже настроены build-tags: при `CGO_ENABLED=0` используется `modernc`, при `=1` — `mattn`.

## Конфигурация

Параметры читаются из окружения (или дефолтов `internal/config/config.go`):

- `BOT_TOKEN` — токен бота.
- `CHAT_IDS` — список chatID, через запятую (например `123456789,-1001234567890`).
- `DB_PATH` — путь к SQLite (например `/var/lib/shoppingbot/shopping.db`).
- `ADMIN_CHAT_ID` — chatID админ-чата (только там доступен режим «быстрое удаление»).
- `BACKUP_EVERY_DAYS` — период бэкапов (целое; `0` = выкл).

Запуск локально:

```bash
export BOT_TOKEN="123:ABC"
export CHAT_IDS="123456789"
export DB_PATH="$PWD/shopping.db"
export ADMIN_CHAT_ID="123456789"
export BACKUP_EVERY_DAYS="7"

./shoppingbot
```

> **Как узнать chatID?** Напишите боту из нужного чата/лички и посмотрите ID через @userinfobot (или залогируйте сами).

## Команды и кнопки

- `/start` — показать доску.
- `/two`, `/three`, `/four` — колонки (пер-чат).
- `/filter all|not|done` — фильтр (или кнопками).
- `/export` — экспорт **всего** списка в ```…``` (`[x]` куплено, `[]` не куплено).
- `/log` — последние записи журнала.
- `/addcat <имена…>` — добавить категории (разделители: `, ; |` или переносы).
- `/delcat <имена…>` — удалить категории.

**Кнопки/режимы:**

- Тумблеры **☐ / ✅** — переключают состояние категории.
- «2/3/4 колонки» — ширина клавиатуры для **текущего чата**.
- Фильтры: **Все / ☐ Не куплено / ✅ Куплено**.
- **↕️ Порядок** — режим сортировки: строки `⬆️  ≡ Категория  ⬇️`.  
  Клик по `≡ Категория` — переместить **в самый верх**; ⬆️/⬇️ — на один шаг.
- **➕ Категория** — инлайн-добавление: введите имя, выберите карточку, отправится `/addcat …`.
- **🗑 Удалить** (только в админ-чате) — «быстрое удаление»:  
  кнопка `✖ Имя` → превращается в `❗Подтвердить: Имя` → повторный клик удаляет.

## Схема БД

- `categories(name PRIMARY KEY, ord INT)` — список и порядок (общий для всех чатов).  
- `selected(category PRIMARY KEY, is_on INT)` — отмеченные (true = **в списке** = не куплено).  
- `settings(key PRIMARY KEY, value TEXT)` — настройки (фильтр, `cols/page/reorder` пер-чат и т.п.).  
- `journal(ts, chat_id, user, category, from_on, to_on)` — журнал (включая имя категории).

При первом запуске, если `categories` пустая, бот засевает дефолтный набор.

## Бэкап

Раз в `BACKUP_EVERY_DAYS` отправляется в `ADMIN_CHAT_ID`:
1) экспорт текущего списка в ```…```,  
2) файл БД (`DB_PATH`) как документ.

---

## systemd (прод-запуск)

### Файл окружения `/etc/default/shoppingbot`

```dotenv
BOT_TOKEN=123:ABC
CHAT_IDS=123456789,-1001234567890
DB_PATH=/var/lib/shoppingbot/shopping.db
ADMIN_CHAT_ID=123456789
BACKUP_EVERY_DAYS=7
# Если собирали с CGO, при необходимости:
# LD_LIBRARY_PATH=/usr/lib
```

### Unit-file `/etc/systemd/system/shoppingbot.service`

```ini
[Unit]
Description=ShoppingBot — Telegram список покупок (Go)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=shoppingbot
Group=shoppingbot
WorkingDirectory=/opt/shoppingbot
ExecStart=/opt/shoppingbot/shoppingbot
EnvironmentFile=/etc/default/shoppingbot

# Безопасность
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ReadWritePaths=/var/lib/shoppingbot

# Поведение
Restart=on-failure
RestartSec=5s
KillMode=process

[Install]
WantedBy=multi-user.target
```

### Установка под systemd (кратко)

```bash
# 1) Пользователь и каталоги
sudo useradd --system --no-create-home --shell /usr/sbin/nologin shoppingbot || true
sudo mkdir -p /opt/shoppingbot /var/lib/shoppingbot
sudo chown -R shoppingbot:shoppingbot /opt/shoppingbot /var/lib/shoppingbot

# 2) Бинарь
sudo cp ./shoppingbot /opt/shoppingbot/shoppingbot
sudo chown shoppingbot:shoppingbot /opt/shoppingbot/shoppingbot
sudo chmod 0755 /opt/shoppingbot/shoppingbot

# 3) Конфиги
sudo tee /etc/default/shoppingbot >/dev/null <<'ENVVARS'
BOT_TOKEN=123:ABC
CHAT_IDS=123456789,-1001234567890
DB_PATH=/var/lib/shoppingbot/shopping.db
ADMIN_CHAT_ID=123456789
BACKUP_EVERY_DAYS=7
ENVVARS

sudo tee /etc/systemd/system/shoppingbot.service >/dev/null <<'UNIT'
[Unit]
Description=ShoppingBot — Telegram список покупок (Go)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=shoppingbot
Group=shoppingbot
WorkingDirectory=/opt/shoppingbot
ExecStart=/opt/shoppingbot/shoppingbot
EnvironmentFile=/etc/default/shoppingbot
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ReadWritePaths=/var/lib/shoppingbot
Restart=on-failure
RestartSec=5s
KillMode=process

[Install]
WantedBy=multi-user.target
UNIT

# 4) Запуск
sudo systemctl daemon-reload
sudo systemctl enable --now shoppingbot.service
sudo systemctl status shoppingbot.service
journalctl -u shoppingbot.service -f
```

## Быстрый чек

1) Соберите: `CGO_ENABLED=0 go build -o shoppingbot ./cmd/bot`.  
2) Настройте `/etc/default/shoppingbot`, включите сервис.  
3) В чате: `/start` → тумблеры; «2/3/4 колонки»; фильтры.  
4) **↕️ Порядок**: `⬆️ / ⬇️ / ≡ Категория` (клик по названию — в топ).  
5) **➕ Категория** → введите «Творог» → карточка → добавится.  
6) В админ-чате: **🗑 Удалить** → `✖ Мука` → `❗Подтвердить: Мука` → удалена.  
7) `/export` — блок со всеми позициями (`[x]/[]`).  
8) `/log` — строки вида:  
   ```
   2025-08-17 19:12  @vasya  [Хлеб]      ☐ → ✅  (123456789)
   2025-08-17 19:10  @masha  [Молочное]  ✅ → ☐  (123456789)
   ```

## Лицензия

MIT (или укажите свою).
