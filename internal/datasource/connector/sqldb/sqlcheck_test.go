package sqldb

import (
	"reflect"
	"strings"
	"testing"
)

func TestCheckMySQLAcceptsReadOnlySelects(t *testing.T) {
	cases := map[string][]tableKey{
		"SELECT id, name FROM users WHERE id = 1": {{"shop", "users"}},
		"SELECT o.id FROM shop.orders o JOIN users u ON u.id = o.user_id": {
			{"shop", "orders"}, {"shop", "users"},
		},
		"WITH recent AS (SELECT * FROM orders) SELECT count(*) FROM recent": {{"shop", "orders"}},
		"SELECT 1": nil,
		"SELECT id FROM users UNION ALL SELECT id FROM orders": {{"shop", "users"}, {"shop", "orders"}},
	}
	for sqlText, want := range cases {
		got, err := checkMySQLSelect(sqlText, "shop")
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", sqlText, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: tables = %v, want %v", sqlText, got, want)
		}
	}
}

func TestCheckMySQLRejectsWritesLocksAndUnsafeCalls(t *testing.T) {
	cases := map[string]string{
		"DELETE FROM users":                                    "SELECT",
		"UPDATE users SET name = 'x'":                          "SELECT",
		"DROP TABLE users":                                     "SELECT",
		"SELECT 1; DELETE FROM users":                          "一次只能执行一条语句",
		"SELECT * FROM users FOR UPDATE":                       "锁定读",
		"SELECT * FROM users LOCK IN SHARE MODE":               "锁定读",
		"SELECT * FROM users INTO OUTFILE '/tmp/x'":            "INTO",
		"SELECT SLEEP(10)":                                     "sleep",
		"SELECT GET_LOCK('a', 10)":                             "get_lock",
		"SELECT LOAD_FILE('/etc/passwd')":                      "load_file",
		"SELECT * FROM users WHERE id = (SELECT 1 FOR UPDATE)": "锁定读",
		"this is not sql":                                      "无法解析",
	}
	for sqlText, reason := range cases {
		_, err := checkMySQLSelect(sqlText, "shop")
		if err == nil {
			t.Fatalf("%q: want rejection (%s)", sqlText, reason)
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(reason)) {
			t.Fatalf("%q: error %q does not mention %q", sqlText, err, reason)
		}
	}
}

func TestCheckPostgresAcceptsQualifiedSelects(t *testing.T) {
	cases := map[string][]tableKey{
		`SELECT id FROM public.users WHERE id = 1`:                      {{"public", "users"}},
		`WITH r AS (SELECT * FROM sales.orders) SELECT count(*) FROM r`: {{"sales", "orders"}},
		`SELECT 1`: nil,
	}
	for sqlText, want := range cases {
		got, err := checkPostgresSelect(sqlText)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", sqlText, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: tables = %v, want %v", sqlText, got, want)
		}
	}
}

func TestCheckPostgresRejectsUnsafeStatements(t *testing.T) {
	cases := map[string]string{
		`DELETE FROM public.users`:                                                    "SELECT",
		`SELECT * INTO public.copy FROM public.users`:                                 "INTO",
		`SELECT * FROM public.users FOR UPDATE`:                                       "锁定读",
		`SELECT pg_sleep(10)`:                                                         "pg_sleep",
		`SELECT lo_import('/etc/passwd')`:                                             "lo_import",
		`SELECT * FROM dblink('dbname=x', 'select 1') AS t(a int)`:                    "dblink",
		`SELECT nextval('public.seq')`:                                                "nextval",
		`WITH d AS (DELETE FROM public.users RETURNING *) SELECT * FROM d`:            "DeleteStmt",
		`WITH i AS (INSERT INTO public.users VALUES (1) RETURNING *) SELECT * FROM i`: "InsertStmt",
		`SELECT * FROM users`:                                                         "schema.table",
		`SELECT 1; SELECT 2`:                                                          "一次只能执行一条语句",
		`not sql at all`:                                                              "无法解析",
	}
	for sqlText, reason := range cases {
		_, err := checkPostgresSelect(sqlText)
		if err == nil {
			t.Fatalf("%q: want rejection (%s)", sqlText, reason)
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(reason)) {
			t.Fatalf("%q: error %q does not mention %q", sqlText, err, reason)
		}
	}
}

func TestExtractedTablesReadsCursorKeys(t *testing.T) {
	cur := cursor{Tables: map[string]string{
		`["shop","users"]`:  "fp1",
		`["shop","orders"]`: "fp2",
		`not json`:          "bad",
	}}
	got := extractedTables(cur)
	want := map[tableKey]bool{{"shop", "users"}: true, {"shop", "orders"}: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractedTables = %v, want %v", got, want)
	}
}
