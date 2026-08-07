package main

import (
	"database/sql"
	"encoding/json"
	"encoding/xml"
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

type exportRecord struct {
	Record
	Kind      string
	spentTime time.Time
}

func transactionLabel(kind string) string {
	if kind == "income" {
		return "Прибыль"
	}
	return "Расход"
}

func escapeXML(value string) string {
	var result strings.Builder
	_ = xml.EscapeText(&result, []byte(value))
	return result.String()
}

func excelCell(value, dataType, style string) string {
	styleAttribute := ""
	if style != "" {
		styleAttribute = ` ss:StyleID="` + style + `"`
	}
	return `<Cell` + styleAttribute + `><Data ss:Type="` + dataType + `">` + escapeXML(value) + `</Data></Cell>`
}

func exportHandler(store transactionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
			return
		}

		typeFilter := r.URL.Query().Get("type")
		if typeFilter == "" {
			typeFilter = "all"
		}
		if typeFilter != "all" && typeFilter != "expense" && typeFilter != "income" {
			http.Error(w, "Некорректный тип операции", http.StatusBadRequest)
			return
		}

		parseBound := func(value string) (*time.Time, error) {
			if value == "" {
				return nil, nil
			}
			parsed, err := time.ParseInLocation("2006-01-02", value, appLocation)
			return &parsed, err
		}
		from, err := parseBound(r.URL.Query().Get("from"))
		if err != nil {
			http.Error(w, "Некорректная начальная дата", http.StatusBadRequest)
			return
		}
		to, err := parseBound(r.URL.Query().Get("to"))
		if err != nil {
			http.Error(w, "Некорректная конечная дата", http.StatusBadRequest)
			return
		}
		if to != nil {
			end := to.AddDate(0, 0, 1)
			to = &end
		}

		query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
		kinds := []string{"expense", "income"}
		if typeFilter != "all" {
			kinds = []string{typeFilter}
		}
		items := make([]exportRecord, 0)
		for _, kind := range kinds {
			records, err := store.list(kind)
			if err != nil {
				http.Error(w, "Не удалось подготовить Excel", http.StatusInternalServerError)
				return
			}
			for _, item := range records {
				spentAt, err := time.ParseInLocation(timeLayout, item.SpentAt, appLocation)
				if err != nil {
					continue
				}
				matchesQuery := query == "" || strings.Contains(strings.ToLower(item.Comment), query) || strings.Contains(strings.ToLower(transactionLabel(kind)), query)
				if !matchesQuery || (from != nil && spentAt.Before(*from)) || (to != nil && !spentAt.Before(*to)) {
					continue
				}
				items = append(items, exportRecord{Record: item, Kind: kind, spentTime: spentAt})
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].spentTime.After(items[j].spentTime) })

		filterLabel := "Все операции"
		if typeFilter == "expense" {
			filterLabel = "Траты"
		} else if typeFilter == "income" {
			filterLabel = "Прибыль"
		}
		queryLabel := strings.TrimSpace(r.URL.Query().Get("query"))
		if queryLabel == "" {
			queryLabel = "Без поиска"
		}
		fromLabel := r.URL.Query().Get("from")
		if fromLabel == "" {
			fromLabel = "Без ограничения"
		}
		toLabel := r.URL.Query().Get("to")
		if toLabel == "" {
			toLabel = "Без ограничения"
		}

		var expenseTotal, incomeTotal float64
		var rows strings.Builder
		for _, item := range items {
			amount := item.Amount
			style := "Income"
			if item.Kind == "expense" {
				expenseTotal += item.Amount
				amount = -item.Amount
				style = "Expense"
			} else {
				incomeTotal += item.Amount
			}
			rows.WriteString(`<Row>`)
			rows.WriteString(excelCell(transactionLabel(item.Kind), "String", ""))
			rows.WriteString(excelCell(item.Comment, "String", ""))
			rows.WriteString(excelCell(item.SpentAt, "String", ""))
			rows.WriteString(excelCell(strconv.FormatFloat(amount, 'f', 2, 64), "Number", style))
			rows.WriteString(`</Row>`)
		}

		var workbook strings.Builder
		workbook.WriteString(`<?xml version="1.0" encoding="UTF-8"?><?mso-application progid="Excel.Sheet"?>`)
		workbook.WriteString(`<Workbook xmlns="urn:schemas-microsoft-com:office:spreadsheet" xmlns:o="urn:schemas-microsoft-com:office:office" xmlns:x="urn:schemas-microsoft-com:office:excel" xmlns:ss="urn:schemas-microsoft-com:office:spreadsheet">`)
		workbook.WriteString(`<Styles><Style ss:ID="Default" ss:Name="Normal"><Alignment ss:Vertical="Center"/><Font ss:FontName="Arial" ss:Size="10"/></Style><Style ss:ID="Title"><Font ss:FontName="Arial" ss:Size="16" ss:Bold="1"/></Style><Style ss:ID="Header"><Font ss:FontName="Arial" ss:Bold="1"/><Interior ss:Color="#E7EBE8" ss:Pattern="Solid"/></Style><Style ss:ID="Income"><Font ss:Color="#157545"/><NumberFormat ss:Format="+# ##0.00 &quot;₸&quot;;-# ##0.00 &quot;₸&quot;"/></Style><Style ss:ID="Expense"><Font ss:Color="#A54D46"/><NumberFormat ss:Format="+# ##0.00 &quot;₸&quot;;-# ##0.00 &quot;₸&quot;"/></Style><Style ss:ID="Total"><Font ss:Bold="1"/><NumberFormat ss:Format="# ##0.00 &quot;₸&quot;"/></Style></Styles>`)
		workbook.WriteString(`<Worksheet ss:Name="Операции"><Table><Column ss:Width="85"/><Column ss:Width="250"/><Column ss:Width="125"/><Column ss:Width="105"/>`)
		workbook.WriteString(`<Row ss:Height="26">` + excelCell("Финансовые операции", "String", "Title") + `</Row>`)
		workbook.WriteString(`<Row>` + excelCell("Фильтр", "String", "") + excelCell(filterLabel, "String", "") + `</Row>`)
		workbook.WriteString(`<Row>` + excelCell("Поиск", "String", "") + excelCell(queryLabel, "String", "") + `</Row>`)
		workbook.WriteString(`<Row>` + excelCell("Период", "String", "") + excelCell(fromLabel+" — "+toLabel, "String", "") + `</Row><Row/>`)
		workbook.WriteString(`<Row>` + excelCell("Тип", "String", "Header") + excelCell("Комментарий", "String", "Header") + excelCell("Дата и время", "String", "Header") + excelCell("Сумма", "String", "Header") + `</Row>`)
		workbook.WriteString(rows.String())
		workbook.WriteString(`<Row/><Row>` + excelCell("Расходы", "String", "") + excelCell("", "String", "") + excelCell("", "String", "") + excelCell(strconv.FormatFloat(expenseTotal, 'f', 2, 64), "Number", "Total") + `</Row>`)
		workbook.WriteString(`<Row>` + excelCell("Прибыль", "String", "") + excelCell("", "String", "") + excelCell("", "String", "") + excelCell(strconv.FormatFloat(incomeTotal, 'f', 2, 64), "Number", "Total") + `</Row>`)
		workbook.WriteString(`<Row>` + excelCell("Чистый результат", "String", "") + excelCell("", "String", "") + excelCell("", "String", "") + excelCell(strconv.FormatFloat(incomeTotal-expenseTotal, 'f', 2, 64), "Number", "Total") + `</Row>`)
		workbook.WriteString(`</Table><WorksheetOptions xmlns="urn:schemas-microsoft-com:office:excel"><FreezePanes/><FrozenNoSplit/><SplitHorizontal>6</SplitHorizontal><TopRowBottomPane>6</TopRowBottomPane><ActivePane>2</ActivePane></WorksheetOptions></Worksheet></Workbook>`)

		w.Header().Set("Content-Type", "application/vnd.ms-excel; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="finansy-%s.xls"`, time.Now().In(appLocation).Format("2006-01-02")))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(workbook.String()))
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
	mux.HandleFunc("/api/export", exportHandler(store))
	mux.Handle("/", http.FileServer(http.Dir("web")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("Трекер финансов запущен на %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
