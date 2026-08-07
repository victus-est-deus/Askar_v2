package main

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type exportTestStore struct {
	records map[string][]Record
}

func (s *exportTestStore) list(kind string) ([]Record, error) {
	return append([]Record(nil), s.records[kind]...), nil
}

func (s *exportTestStore) add(string, Record) (Record, error) { return Record{}, nil }
func (s *exportTestStore) remove(string, int64) (bool, error) { return false, nil }
func (s *exportTestStore) close() error                       { return nil }

func TestExportHandlerAppliesFilters(t *testing.T) {
	store := &exportTestStore{records: map[string][]Record{
		"expense": {
			{ID: 1, Amount: 1250, Comment: "Корм & вода", SpentAt: "10:30 07.08.2026"},
			{ID: 2, Amount: 800, Comment: "Корм старый", SpentAt: "10:30 06.08.2026"},
		},
		"income": {
			{ID: 1, Amount: 5000, Comment: "Корм продажа", SpentAt: "11:00 07.08.2026"},
		},
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/export?type=expense&query=корм&from=2026-08-07&to=2026-08-07", nil)
	response := httptest.NewRecorder()

	exportHandler(store).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", response.Code)
	}
	if !strings.Contains(response.Header().Get("Content-Disposition"), ".xls") {
		t.Fatalf("expected Excel attachment, got %q", response.Header().Get("Content-Disposition"))
	}
	body := response.Body.String()
	if !strings.Contains(body, "Корм &amp; вода") {
		t.Fatal("expected matching expense with escaped XML")
	}
	if strings.Contains(body, "Корм старый") || strings.Contains(body, "Корм продажа") {
		t.Fatal("export contains records excluded by active filters")
	}
	if !strings.Contains(body, `<Data ss:Type="Number">-1250.00</Data>`) {
		t.Fatal("expected expense amount to be exported as a negative number")
	}
	if err := xml.Unmarshal(response.Body.Bytes(), &struct{}{}); err != nil {
		t.Fatalf("export is not valid XML: %v", err)
	}
}

func TestExportHandlerRejectsInvalidDate(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/export?from=07.08.2026", nil)
	response := httptest.NewRecorder()

	exportHandler(&exportTestStore{}).ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", response.Code)
	}
}
