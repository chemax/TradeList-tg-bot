# README — ShoppingBot (Go + Telegram)

Минималистичный Telegram-бот «механический список покупок». Главный экран — **только категории и пагинация**; всё остальное спрятано в **подменю**.

## Возможности

- Бинарные тумблеры **☐ / ✅** (не куплено / куплено).
- Фильтры: **Все / ☑ Куплено / ☐ Не куплено**.
- **2/3/4 колонки** — настраиваются **пер-чат**.
- **Подменю**: на главном экране — только категории и пагинация; действия в «⋯ Меню».
- **Порядок категорий**: режим «↕️ Порядок» (кнопки `⬆️`/`⬇️`), клик по `≡ Категория` — поднимает категорию **в самый верх**. Порядок общий для всех чатов (хранится в SQLite).
- Управление категориями из чата: `/addcat`, `/delcat`, **инлайн-добавление** («➕ Категория»).
- **Админ-режим «быстрое удаление»**: `✖ Имя` → «❗Подтвердить: Имя» → удаление (только в админ-чате).
- **«☐ Всё»** — отметить **все** категории как «не куплено»; в журнал пишется **агрегированная запись** вида `ALL → ☐ (+N)`.
- **Число строк на страницу** — пер-чат: в подменю кнопки **`−8` · `Строк N` · `+8`** (диапазон 4…48).
- **Автопагинация** «только если нужно» (учитывает колонки и выбранное число строк).
- **Экспорт** (весь список) в ```…``` с `[x]/[]`.
- **Журнал** изменений с именем категории + агрегированные записи.
- **Бэкапы**: авто раз в `N` дней + **ручной** (кнопка «💾 Бэкап» в админ-чате или `/backup`). В админ-чат отправляются экспорт и файл БД.
- Техническое: защита от «message is not modified», дебаунс рассылки на `time.AfterFunc`, SQLite-драйвер **pure Go** (`modernc.org/sqlite`) с альтернативой CGO (`mattn/go-sqlite3`).

## Архитектура

```
shoppingbot/
├─ cmd/bot/main.go                # старт и конфиг
├─ internal/
│  ├─ config/config.go            # BOT_TOKEN, CHAT_IDS, DB_PATH, ADMIN_CHAT_ID, BACKUP_EVERY_DAYS
│  ├─ list/list.go                # доменная логика (категории/фильтр/экспорт/видимость)
│  ├─ store/sqlite.go             # SQLite: категории/selected/настройки/журнал/перемещения/массовые операции
│  ├─ store/sqlite_driver_cgo.go  # //go:build cgo   -> github.com/mattn/go-sqlite3
│  ├─ store/sqlite_driver_pure.go # //go:build !cgo  -> modernc.org/sqlite (pure Go)
│  └─ tg/tg.go                    # Telegram-слой, подменю, пагинация, колонки, бэкап, журнал
```

## Требования

- Go **1.22+**
- Бот в Telegram (@BotFather), **включён inline-режим** (`/setinline` → Enable).
- Linux (для запуска через systemd).

## Сборка

### Вариант A — без CGO (по умолчанию, кросс-платформенно)

Используется `modernc.org/sqlite` (pure Go).

```bash
export CGO_ENABLED=0
go mod tidy
go build -o shoppingbot ./cmd/bot
```

### Вариант B — с CGO (чуть быстрее на больших БД)

Используется `github.com/mattn/go-sqlite3`.

```bash
# Debian/Ubuntu: sudo apt-get install -y build-essential
# Alpine:        sudo apk add --no-cache build-base
export CGO_ENABLED=1
go build -o shoppingbot ./cmd/bot
```

> Build-tags в проекте уже настроены: при `CGO_ENABLED=0` берётся `modernc`, при `=1` — `mattn`.

## Конфигурация

Через окружение (или дефолты `internal/config/config.go`):

- `BOT_TOKEN` — токен бота.
- `CHAT_IDS` — список chatID через запятую (например `123456789,-1001234567890`).
- `DB_PATH` — путь к SQLite (например `/var/lib/shoppingbot/shopping.db`).
- `ADMIN_CHAT_ID` — chatID админ-чата (там доступны «быстрое удаление» и «бэкап»).
- `BACKUP_EVERY_DAYS` — период автобэкапов (целое; `0` = выключено).

> **Число строк на страницу** задаётся **пер-чат** из подменю `−8 / Строк N / +8`. Базовый дефолт — 12 (или `SoftMaxRows` из конфигурации запуска, если он переопределён).

### Пример локального запуска

```bash
export BOT_TOKEN="123:ABC"
export CHAT_IDS="123456789"
export DB_PATH="$PWD/shopping.db"
export ADMIN_CHAT_ID="123456789"
export BACKUP_EVERY_DAYS="7"

./shoppingbot
```

## Команды и кнопки

- `/start` — показать доску.
- `/two`, `/three`, `/four` — колонки (пер-чат).
- `/filter all|not|done` — фильтр (или кнопками).
- `/export` — экспорт **всего** списка в ```…``` (`[x]` куплено, `[]` не куплено).
- `/log` — последние записи журнала.
- `/addcat <имена…>` — добавить категории (разделители: `, ; |` или переносы).
- `/delcat <имена…>` — удалить категории.
- `/backup` — ручной бэкап (только в админ-чате).

**Главный экран:** только тумблеры категорий и **пагинация** (если нужна).  
**Подменю («⋯ Меню»):**
- Фильтры: **Все / ☐ Не куплено / ✅ Куплено**;
- Колонки: **2 / 3 / 4**;
- Строки: **`−8` · `Строк N` · `+8`** (диапазон 4…48);
- **➕ Категория** (инлайн-добавление);
- **☐ Всё** (отметить все как «не куплено», агрегированная запись в журнале);
- **📤 Экспорт**, **📜 Журнал**;
- **↕️ Порядок** (войти/выйти из режима);
- (Только админ-чат) **🗑 Удалить**, **💾 Бэкап**.

**Режим «↕️ Порядок»:**
- Строки вида: `⬆️  ≡ Категория  ⬇️`;
- Нажатие на `≡ Категория` — **в топ**;
- «✅ Готово» — выход из режима.

**Админ-режим «быстрое удаление»:**
- `✖ Имя` → «❗Подтвердить: Имя» → удаление;
- «↩️ Назад» — выход.

## Схема БД

- `categories(name PRIMARY KEY, ord INT)` — список и порядок (общий, глобальный).
- `selected(category PRIMARY KEY, is_on INT)` — отмеченные (true = **в списке** = не куплено).
- `settings(key PRIMARY KEY, value TEXT)` — настройки (фильтр, `cols/page/reorder/rows` пер-чат и пр.).
- `journal(ts, chat_id, user, category, from_on, to_on)` — журнал; для массовых операций категория может быть вида `ALL → ☐ (+N)`.

При первом запуске, если `categories` пустая, бот засевает дефолтный набор.

## Бэкап

- **Авто**: раз в `BACKUP_EVERY_DAYS` отправляются:
    1) экспорт текущего списка в ```…```,
    2) файл БД (`DB_PATH`) как документ в `ADMIN_CHAT_ID`.
- **Ручной**: кнопка «💾 Бэкап» (в подменю админ-чата) или команда `/backup`.

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

1) `/start` → тумблеры; «⋯ Меню»; пагинация (если нужна).
2) В «⋯ Меню»: фильтры, **2/3/4**, **−8 / Строк N / +8**, **➕ Категория**, **☐ Всё**, **📤 Экспорт**, **📜 Журнал**, **↕️ Порядок**.
3) Клик по `≡ Категория` в режиме порядка — перемещение в **топ**.
4) В админ-чате — «🗑 Удалить», «💾 Бэкап»; команда `/backup`.
5) `/export` — блок со всеми позициями (`[x]/[]`).
6) `/log` — строки вида:
   ```
   2025-08-17 21:02  @енот  [ALL → ☐ (+7)]  ✅ → ☐  (123456789)
   2025-08-17 19:12  @vasya [Хлеб]         ☐ → ✅  (123456789)
   ```

## Лицензия

MIT (или укажите свою).
