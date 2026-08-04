package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

type Record struct {
	ID      int64   `json:"id"`
	Amount  float64 `json:"amount"`
	Comment string  `json:"comment"`
	SpentAt string  `json:"spentAt"`
}

type transactionStore interface {
	list(string) ([]Record, error)
	add(string, Record) (Record, error)
	remove(string, int64) (bool, error)
	close() error
}

func tableForKind(kind string) (string, error) {
	switch kind {
	case "expense":
		return "expenses", nil
	case "income":
		return "incomes", nil
	default:
		return "", fmt.Errorf("unknown transaction kind: %s", kind)
	}
}

type jsonStore struct {
	mu            sync.Mutex
	expenses      []Record
	incomes       []Record
	nextExpenseID int64
	nextIncomeID  int64
	dataDir       string
}

func newJSONStore(dataDir string) (*jsonStore, error) {
	s := &jsonStore{dataDir: dataDir, nextExpenseID: 1, nextIncomeID: 1}
	if err := loadJSONRecords(filepath.Join(dataDir, "expenses.json"), &s.expenses, &s.nextExpenseID); err != nil {
		return nil, err
	}
	if err := loadJSONRecords(filepath.Join(dataDir, "incomes.json"), &s.incomes, &s.nextIncomeID); err != nil {
		return nil, err
	}
	return s, nil
}

func loadJSONRecords(path string, records *[]Record, nextID *int64) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, records); err != nil {
		return err
	}
	for _, item := range *records {
		if item.ID >= *nextID {
			*nextID = item.ID + 1
		}
	}
	return nil
}

func (s *jsonStore) records(kind string) (*[]Record, *int64, string, error) {
	switch kind {
	case "expense":
		return &s.expenses, &s.nextExpenseID, filepath.Join(s.dataDir, "expenses.json"), nil
	case "income":
		return &s.incomes, &s.nextIncomeID, filepath.Join(s.dataDir, "incomes.json"), nil
	default:
		return nil, nil, "", fmt.Errorf("unknown transaction kind: %s", kind)
	}
}

func saveJSONRecords(path string, records []Record) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (s *jsonStore) list(kind string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _, _, err := s.records(kind)
	if err != nil {
		return nil, err
	}
	items := append([]Record(nil), (*records)...)
	sort.Slice(items, func(i, j int) bool {
		a, _ := time.ParseInLocation(timeLayout, items[i].SpentAt, appLocation)
		b, _ := time.ParseInLocation(timeLayout, items[j].SpentAt, appLocation)
		return a.After(b)
	})
	return items, nil
}

func (s *jsonStore) add(kind string, item Record) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, nextID, path, err := s.records(kind)
	if err != nil {
		return Record{}, err
	}
	item.ID = *nextID
	*nextID++
	*records = append(*records, item)
	if err := saveJSONRecords(path, *records); err != nil {
		return Record{}, err
	}
	return item, nil
}

func (s *jsonStore) remove(kind string, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _, path, err := s.records(kind)
	if err != nil {
		return false, err
	}
	for i, item := range *records {
		if item.ID == id {
			*records = append((*records)[:i], (*records)[i+1:]...)
			return true, saveJSONRecords(path, *records)
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
	for _, table := range []string{"expenses", "incomes"} {
		statement := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id BIGSERIAL PRIMARY KEY,
			amount NUMERIC(14, 2) NOT NULL CHECK (amount > 0),
			comment VARCHAR(100) NOT NULL,
			spent_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`, table)
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &postgresStore{db: db}, nil
}

func (s *postgresStore) list(kind string) ([]Record, error) {
	table, err := tableForKind(kind)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(fmt.Sprintf(`SELECT id, amount, comment, spent_at FROM %s ORDER BY spent_at DESC, id DESC`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Record, 0)
	for rows.Next() {
		var item Record
		var spentAt time.Time
		if err := rows.Scan(&item.ID, &item.Amount, &item.Comment, &spentAt); err != nil {
			return nil, err
		}
		item.SpentAt = spentAt.In(appLocation).Format(timeLayout)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *postgresStore) add(kind string, item Record) (Record, error) {
	table, err := tableForKind(kind)
	if err != nil {
		return Record{}, err
	}
	spentAt, err := time.ParseInLocation(timeLayout, item.SpentAt, appLocation)
	if err != nil {
		return Record{}, err
	}
	err = s.db.QueryRow(
		fmt.Sprintf(`INSERT INTO %s (amount, comment, spent_at) VALUES ($1, $2, $3) RETURNING id`, table),
		item.Amount, item.Comment, spentAt,
	).Scan(&item.ID)
	return item, err
}

func (s *postgresStore) remove(kind string, id int64) (bool, error) {
	table, err := tableForKind(kind)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table), id)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *postgresStore) close() error { return s.db.Close() }

func openStore() (transactionStore, error) {
	if databaseURL := os.Getenv("DATABASE_URL"); databaseURL != "" {
		log.Print("Используется PostgreSQL")
		return newPostgresStore(databaseURL)
	}
	if err := os.MkdirAll("data", 0755); err != nil {
		return nil, err
	}
	log.Print("DATABASE_URL не задан, используется локальное JSON-хранилище")
	return newJSONStore("data")
}

func collectionHandler(store transactionStore, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch r.Method {
		case http.MethodGet:
			items, err := store.list(kind)
			if err != nil {
				http.Error(w, "Не удалось загрузить записи", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(items)
		case http.MethodPost:
			var item Record
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
			created, err := store.add(kind, item)
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
	}
}

func itemHandler(store transactionStore, kind, pathPrefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
			return
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, pathPrefix), 10, 64)
		if err != nil {
			http.Error(w, "Некорректный id", http.StatusBadRequest)
			return
		}
		ok, err := store.remove(kind, id)
		if err != nil {
			http.Error(w, "Не удалось удалить запись", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func main() {
	store, err := openStore()
	if err != nil {
		log.Fatal(err)
	}
	defer store.close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/expenses", collectionHandler(store, "expense"))
	mux.HandleFunc("/api/expenses/", itemHandler(store, "expense", "/api/expenses/"))
	mux.HandleFunc("/api/incomes", collectionHandler(store, "income"))
	mux.HandleFunc("/api/incomes/", itemHandler(store, "income", "/api/incomes/"))
	mux.Handle("/", http.FileServer(http.Dir("web")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("Трекер финансов запущен на %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
