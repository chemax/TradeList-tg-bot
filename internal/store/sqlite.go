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
	// Включаем внешние ключи на новых БД
	if _, err := s.db.Exec(`PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL;`); err != nil {
		return err
	}

	// Таблицы
	if _, err := s.db.Exec(`
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
`); err != nil {
		return err
	}

	return nil
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
	// получим следующий ord
	var maxOrd sql.NullInt64
	_ = s.db.QueryRow(`SELECT MAX(ord) FROM categories`).Scan(&maxOrd)
	next := 1
	if maxOrd.Valid {
		next = int(maxOrd.Int64) + 1
	}
	// вставим, игнорируя дубликаты
	_, err := s.db.Exec(`INSERT OR IGNORE INTO categories(name, ord) VALUES(?,?)`, name, next)
	return err
}

func (s *Store) DeleteCategory(name string) error {
	if stringsTrim(name) == "" {
		return nil
	}
	// удалим из categories и зачистим selected
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
