package store

import (
	"database/sql"
	_ "github.com/mattn/go-sqlite3"
	"time"
)

type Store struct{ db *sql.DB }

type JournalEntry struct {
	TS       time.Time
	ChatID   int64
	User     string
	Category string
	From     bool
	To       bool
}

func New(path string) (*Store, error) {
	db, err := sql.Open(sqliteDriver, path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	if _, err := s.db.Exec(`PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL;`); err != nil {
		return err
	}
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS categories (
  name TEXT PRIMARY KEY,
  ord  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS selected (
  category TEXT PRIMARY KEY,
  is_on    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS journal (
  ts       INTEGER NOT NULL,
  chat_id  INTEGER NOT NULL,
  user     TEXT    NOT NULL,
  category TEXT    NOT NULL,
  from_on  INTEGER NOT NULL,
  to_on    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notification_schedule (
  user_id       INTEGER PRIMARY KEY,
  minute_of_day INTEGER, -- 0..1430, шаг 30; NULL => отключено
  updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%S','now'))
);
`)
	return err
}

// ---------- Settings ----------
func (s *Store) GetSetting(key string, def string) (string, error) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return def, nil
	}
	return val, err
}
func (s *Store) SetSetting(key, val string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
	                 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val)
	return err
}

// ---------- Selected ----------
func (s *Store) LoadSelected() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT category, is_on FROM selected`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		var on int
		if err := rows.Scan(&c, &on); err != nil {
			return nil, err
		}
		out[c] = on != 0
	}
	return out, rows.Err()
}
func (s *Store) SetSelected(cat string, on bool) error {
	val := 0
	if on {
		val = 1
	}
	_, err := s.db.Exec(`INSERT INTO selected(category,is_on) VALUES(?,?)
	                     ON CONFLICT(category) DO UPDATE SET is_on=excluded.is_on`, cat, val)
	return err
}

// ---------- Journal ----------
func (s *Store) AppendJournal(e JournalEntry) error {
	_, err := s.db.Exec(`INSERT INTO journal(ts,chat_id,user,category,from_on,to_on)
	                     VALUES(?,?,?,?,?,?)`,
		e.TS.Unix(), e.ChatID, e.User, e.Category, b2i(e.From), b2i(e.To))
	return err
}
func (s *Store) LastJournal(limit int) ([]JournalEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT ts,chat_id,user,category,from_on,to_on
	                         FROM journal ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var ts int64
		var e JournalEntry
		var fromOn, toOn int
		if err := rows.Scan(&ts, &e.ChatID, &e.User, &e.Category, &fromOn, &toOn); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0)
		e.From = fromOn != 0
		e.To = toOn != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- Categories ----------
func (s *Store) ListCategories() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM categories ORDER BY ord ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) AddCategory(name string) error {
	if stringsTrim(name) == "" {
		return nil
	}
	var maxOrd sql.NullInt64
	_ = s.db.QueryRow(`SELECT MAX(ord) FROM categories`).Scan(&maxOrd)
	next := 1
	if maxOrd.Valid {
		next = int(maxOrd.Int64) + 1
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO categories(name, ord) VALUES(?,?)`, name, next)
	return err
}

func (s *Store) DeleteCategory(name string) error {
	if stringsTrim(name) == "" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM categories WHERE name=?`, name); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err = tx.Exec(`DELETE FROM selected   WHERE category=?`, name); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// MoveCategoryUp — поменять местами категорию с предыдущей по ord.
func (s *Store) MoveCategoryUp(name string) error {
	var curOrd int
	err := s.db.QueryRow(`SELECT ord FROM categories WHERE name=?`, name).Scan(&curOrd)
	if err != nil {
		return err
	}
	var nbName string
	var nbOrd int
	// сосед сверху: ord < curOrd (наибольший из меньших)
	err = s.db.QueryRow(`SELECT name, ord FROM categories WHERE ord < ? ORDER BY ord DESC LIMIT 1`, curOrd).Scan(&nbName, &nbOrd)
	if err == sql.ErrNoRows {
		return nil
	} // уже самый верх
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE categories SET ord=-1 WHERE name=?`, name); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err = tx.Exec(`UPDATE categories SET ord=? WHERE name=?`, curOrd, nbName); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err = tx.Exec(`UPDATE categories SET ord=? WHERE name=?`, nbOrd, name); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// MoveCategoryDown — поменять местами категорию со следующей по ord.
func (s *Store) MoveCategoryDown(name string) error {
	var curOrd int
	err := s.db.QueryRow(`SELECT ord FROM categories WHERE name=?`, name).Scan(&curOrd)
	if err != nil {
		return err
	}
	var nbName string
	var nbOrd int
	// сосед снизу: ord > curOrd (наименьший из больших)
	err = s.db.QueryRow(`SELECT name, ord FROM categories WHERE ord > ? ORDER BY ord ASC LIMIT 1`, curOrd).Scan(&nbName, &nbOrd)
	if err == sql.ErrNoRows {
		return nil
	} // уже самый низ
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE categories SET ord=-1 WHERE name=?`, name); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err = tx.Exec(`UPDATE categories SET ord=? WHERE name=?`, curOrd, nbName); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err = tx.Exec(`UPDATE categories SET ord=? WHERE name=?`, nbOrd, name); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// small helper to avoid importing strings everywhere
func stringsTrim(s string) string {
	i := 0
	j := len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	for i < j && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}
	if i >= j {
		return ""
	}
	return s[i:j]
}

// MoveCategoryTop — поднять категорию в самый верх списка, сохранив относительный порядок остальных.
// Делается атомарно: всем ord +1, целевой = 1.
func (s *Store) MoveCategoryTop(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	// сначала сдвигаем всех вниз
	if _, err = tx.Exec(`UPDATE categories SET ord = ord + 1`); err != nil {
		_ = tx.Rollback()
		return err
	}
	// затем ставим выбранную категорию на верх
	if res, err := tx.Exec(`UPDATE categories SET ord = 1 WHERE name = ?`, name); err != nil {
		_ = tx.Rollback()
		return err
	} else {
		if aff, _ := res.RowsAffected(); aff == 0 {
			_ = tx.Rollback()
			return sql.ErrNoRows
		}
	}
	return tx.Commit()
}

// SetAllSelected устанавливает всем категориям признак "в списке" (не куплено) = on.
// Совместимо с любой версией SQLite: без ON CONFLICT DO UPDATE.
// Делается атомарно: UPDATE существующих + INSERT недостающих в одной транзакции.
func (s *Store) SetAllSelected(on bool) error {
	val := 0
	if on {
		val = 1
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}

	// 1) Обновить все уже существующие записи selected для категорий
	if _, err := tx.Exec(`
		UPDATE selected
		   SET is_on = ?
		 WHERE category IN (SELECT name FROM categories)
	`, val); err != nil {
		_ = tx.Rollback()
		return err
	}

	// 2) Вставить недостающие записи selected для категорий, которых ещё нет в selected
	if _, err := tx.Exec(`
		INSERT INTO selected(category, is_on)
		SELECT c.name, ?
		  FROM categories c
		 WHERE NOT EXISTS (SELECT 1 FROM selected s WHERE s.category = c.name)
	`, val); err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

// ----------- Notification schedule -----------

// SetNotification upserts user's minute_of_day (0..1430, step 30).
// If minuteOfDay == nil, it disables notifications (NULL).
func (s *Store) SetNotification(userID int64, minuteOfDay *int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if minuteOfDay == nil {
		_, err = tx.Exec(`
			INSERT INTO notification_schedule(user_id, minute_of_day) VALUES(?, NULL)
			ON CONFLICT(user_id) DO UPDATE
			SET minute_of_day=NULL,
			    updated_at=strftime('%Y-%m-%d %H:%M:%S','now')
		`, userID)
	} else {
		_, err = tx.Exec(`
			INSERT INTO notification_schedule(user_id, minute_of_day) VALUES(?, ?)
			ON CONFLICT(user_id) DO UPDATE
			SET minute_of_day=excluded.minute_of_day,
			    updated_at=strftime('%Y-%m-%d %H:%M:%S','now')
		`, userID, *minuteOfDay)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
