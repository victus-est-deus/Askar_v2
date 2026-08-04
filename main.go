package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

const timeLayout = "15:04 02.01.2006"

var appLocation = time.FixedZone("Asia/Qyzylorda", 5*60*60)

type Expense struct {
	ID      int64   `json:"id"`
	Amount  float64 `json:"amount"`
	Comment string  `json:"comment"`
	SpentAt string  `json:"spentAt"`
}

type expenseStore interface {
	list() ([]Expense, error)
	add(Expense) (Expense, error)
	remove(int64) (bool, error)
	close() error
}

type jsonStore struct {
	mu       sync.Mutex
	expenses []Expense
	nextID   int64
	path     string
}

func newJSONStore(path string) (*jsonStore, error) {
	s := &jsonStore{path: path, nextID: 1}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.expenses); err != nil {
		return nil, err
	}
	for _, item := range s.expenses {
		if item.ID >= s.nextID {
			s.nextID = item.ID + 1
		}
	}
	return s, nil
}

func (s *jsonStore) save() error {
	data, err := json.MarshalIndent(s.expenses, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (s *jsonStore) list() ([]Expense, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := append([]Expense(nil), s.expenses...)
	sort.Slice(items, func(i, j int) bool {
		a, _ := time.ParseInLocation(timeLayout, items[i].SpentAt, appLocation)
		b, _ := time.ParseInLocation(timeLayout, items[j].SpentAt, appLocation)
		return a.After(b)
	})
	return items, nil
}

func (s *jsonStore) add(item Expense) (Expense, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item.ID = s.nextID
	s.nextID++
	s.expenses = append(s.expenses, item)
	if err := s.save(); err != nil {
		return Expense{}, err
	}
	return item, nil
}

func (s *jsonStore) remove(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, item := range s.expenses {
		if item.ID == id {
			s.expenses = append(s.expenses[:i], s.expenses[i+1:]...)
			return true, s.save()
		}
	}
	return false, nil
}

func (s *jsonStore) close() error { return nil }

type postgresStore struct{ db *sql.DB }

func newPostgresStore(databaseURL string) (*postgresStore, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS expenses (
		id BIGSERIAL PRIMARY KEY,
		amount NUMERIC(14, 2) NOT NULL CHECK (amount > 0),
		comment VARCHAR(100) NOT NULL,
		spent_at TIMESTAMPTZ NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &postgresStore{db: db}, nil
}

func (s *postgresStore) list() ([]Expense, error) {
	rows, err := s.db.Query(`SELECT id, amount, comment, spent_at FROM expenses ORDER BY spent_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Expense, 0)
	for rows.Next() {
		var item Expense
		var spentAt time.Time
		if err := rows.Scan(&item.ID, &item.Amount, &item.Comment, &spentAt); err != nil {
			return nil, err
		}
		item.SpentAt = spentAt.In(appLocation).Format(timeLayout)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *postgresStore) add(item Expense) (Expense, error) {
	spentAt, err := time.ParseInLocation(timeLayout, item.SpentAt, appLocation)
	if err != nil {
		return Expense{}, err
	}
	err = s.db.QueryRow(
		`INSERT INTO expenses (amount, comment, spent_at) VALUES ($1, $2, $3) RETURNING id`,
		item.Amount, item.Comment, spentAt,
	).Scan(&item.ID)
	return item, err
}

func (s *postgresStore) remove(id int64) (bool, error) {
	result, err := s.db.Exec(`DELETE FROM expenses WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *postgresStore) close() error { return s.db.Close() }

func openStore() (expenseStore, error) {
	if databaseURL := os.Getenv("DATABASE_URL"); databaseURL != "" {
		log.Print("Используется PostgreSQL")
		return newPostgresStore(databaseURL)
	}
	if err := os.MkdirAll("data", 0755); err != nil {
		return nil, err
	}
	log.Print("DATABASE_URL не задан, используется локальное JSON-хранилище")
	return newJSONStore(filepath.Join("data", "expenses.json"))
}

func main() {
	store, err := openStore()
	if err != nil {
		log.Fatal(err)
	}
	defer store.close()

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("web")))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/expenses", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.Method {
		case http.MethodGet:
			items, err := store.list()
			if err != nil {
				http.Error(w, "Не удалось загрузить записи", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(items)
		case http.MethodPost:
			var item Expense
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&item); err != nil {
				http.Error(w, "Некорректные данные", http.StatusBadRequest)
				return
			}
			item.Comment = strings.TrimSpace(item.Comment)
			if item.Amount <= 0 || item.Comment == "" || len([]rune(item.Comment)) > 100 {
				http.Error(w, "Укажите корректную сумму и комментарий", http.StatusBadRequest)
				return
			}
			if _, err := time.ParseInLocation(timeLayout, item.SpentAt, appLocation); err != nil {
				http.Error(w, "Время должно быть в формате ЧЧ:ММ ДД.ММ.ГГГГ", http.StatusBadRequest)
				return
			}
			created, err := store.add(item)
			if err != nil {
				http.Error(w, "Не удалось сохранить запись", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/expenses/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
			return
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/expenses/"), 10, 64)
		if err != nil {
			http.Error(w, "Некорректный id", http.StatusBadRequest)
			return
		}
		ok, err := store.remove(id)
		if err != nil {
			http.Error(w, "Не удалось удалить запись", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("Трекер трат запущен на %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
