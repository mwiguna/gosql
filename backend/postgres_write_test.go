package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMutationErrorMapping(t *testing.T) {
	for _, item := range []struct {
		err    error
		status int
		code   string
	}{
		{pgx.ErrNoRows, 409, "row_changed"},
		{&pgconn.PgError{Code: "55P03"}, 409, "table_busy"},
		{&pgconn.PgError{Code: "23505"}, 409, "unique_violation"},
		{&pgconn.PgError{Code: "23503"}, 409, "foreign_key_violation"},
		{&pgconn.PgError{Code: "23502"}, 400, "not_null_violation"},
		{&pgconn.PgError{Code: "23514"}, 400, "check_violation"},
		{&pgconn.PgError{Code: "22P02"}, 400, "invalid_value"},
	} {
		response := httptest.NewRecorder()
		writeMutationError(response, item.err)
		var body struct {
			Error apiError `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != item.status || body.Error.Code != item.code {
			t.Fatalf("%v: got %d %s", item.err, response.Code, body.Error.Code)
		}
	}
}
