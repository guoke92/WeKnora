package sqldb

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// Integration tests run against a real MySQL server and are skipped unless the
// environment provides one. Credentials are read from the environment only and are
// never written to the repository.
//
//	WEKNORA_EXTDB_TEST_MYSQL_HOST       required
//	WEKNORA_EXTDB_TEST_MYSQL_PORT       default 3306
//	WEKNORA_EXTDB_TEST_MYSQL_USER       required
//	WEKNORA_EXTDB_TEST_MYSQL_PASSWORD   may be empty
//	WEKNORA_EXTDB_TEST_MYSQL_DATABASE   required
//	WEKNORA_EXTDB_TEST_MYSQL_TLS        default disable
//	WEKNORA_EXTDB_TEST_MAX_TABLES       default 3 (keeps the load on the server small)
//	WEKNORA_EXTDB_TEST_DUMP             optional directory to write rendered documents to
//
// The server host must also be allowed by SSRF_WHITELIST_EXTRA when it is a private
// address, because every connection dials through the SSRF-safe dialer.
func mysqlIntegrationConfig(t *testing.T) *types.DataSourceConfig {
	t.Helper()
	host := os.Getenv("WEKNORA_EXTDB_TEST_MYSQL_HOST")
	if host == "" {
		t.Skip("WEKNORA_EXTDB_TEST_MYSQL_HOST is not set; skipping MySQL integration test")
	}
	port, err := strconv.Atoi(envOr("WEKNORA_EXTDB_TEST_MYSQL_PORT", "3306"))
	if err != nil {
		t.Fatalf("invalid WEKNORA_EXTDB_TEST_MYSQL_PORT: %v", err)
	}
	maxTables, err := strconv.Atoi(envOr("WEKNORA_EXTDB_TEST_MAX_TABLES", "3"))
	if err != nil {
		t.Fatalf("invalid WEKNORA_EXTDB_TEST_MAX_TABLES: %v", err)
	}
	return &types.DataSourceConfig{
		Type: types.ConnectorTypeMySQL,
		Credentials: map[string]interface{}{
			"user":     os.Getenv("WEKNORA_EXTDB_TEST_MYSQL_USER"),
			"password": os.Getenv("WEKNORA_EXTDB_TEST_MYSQL_PASSWORD"),
		},
		Settings: map[string]interface{}{
			"host":                host,
			"port":                float64(port),
			"database":            os.Getenv("WEKNORA_EXTDB_TEST_MYSQL_DATABASE"),
			"tls_mode":            envOr("WEKNORA_EXTDB_TEST_MYSQL_TLS", "disable"),
			"profile_sample_rows": float64(1000),
			"max_tables":          float64(maxTables),
		},
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func TestMySQLIntegrationSyncAndRerun(t *testing.T) {
	ds := mysqlIntegrationConfig(t)
	if ds.Settings["database"] == "" {
		t.Skip("WEKNORA_EXTDB_TEST_MYSQL_DATABASE is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewMySQLConnector()
	if err := c.Validate(ctx, ds); err != nil {
		t.Fatalf("validate: %v", err)
	}

	database := ds.Settings["database"].(string)
	schemas, err := c.ListResources(ctx, ds, "")
	if err != nil {
		t.Fatalf("list schemas: %v", err)
	}
	if len(schemas) != 1 || schemas[0].Name != database {
		t.Fatalf("schemas = %+v, want only %q", schemas, database)
	}
	tables, err := c.ListResources(ctx, ds, schemas[0].ExternalID)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	t.Logf("database %q has %d base tables", database, len(tables))

	ds.ResourceIDs = []string{schemas[0].ExternalID}
	h1 := &recHandler{}
	cur, err := c.FetchStream(ctx, ds, nil, h1)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	for _, e := range h1.errorItems() {
		t.Logf("sync error item: title=%q code=%s", e.Title, e.Metadata["error_reason_code"])
	}
	docs := h1.emittedIDs()
	if len(docs) == 0 && len(tables) > 0 {
		t.Fatalf("no documents emitted for %d tables", len(tables))
	}
	dumpDir := os.Getenv("WEKNORA_EXTDB_TEST_DUMP")
	for _, it := range h1.items {
		if it.IsDeleted || it.Metadata["error"] != "" {
			continue
		}
		body := string(it.Content)
		if !strings.HasPrefix(body, "# "+database+".") {
			t.Fatalf("document %q has unexpected heading: %.80q", it.Title, body)
		}
		if dumpDir != "" {
			if err := os.MkdirAll(dumpDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dumpDir, it.FileName), it.Content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("rendered %s: %d bytes", it.Title, len(it.Content))
	}

	// A second run over unchanged tables must not re-emit documents. Live tables may
	// legitimately change between runs, so this is reported rather than failed.
	h2 := &recHandler{}
	if _, err := c.FetchStream(ctx, ds, cur, h2); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	t.Logf("second run re-emitted %d of %d documents (0 expected when data is static)", len(h2.emittedIDs()), len(docs))
	if len(h2.deletedIDs()) != 0 {
		t.Fatalf("unchanged selection deleted documents: %v", h2.deletedIDs())
	}
}

// TestMySQLIntegrationReadOnlyProbe writes to a table that does not exist. Inside a
// READ ONLY transaction the server rejects the statement before looking for the table,
// so the error names the read-only restriction. Without read-only enforcement the error
// would be "table doesn't exist" and nothing would be written either way.
func TestMySQLIntegrationReadOnlyProbe(t *testing.T) {
	ds := mysqlIntegrationConfig(t)
	cfg, err := parseConfig(engineMySQL, ds)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, err := openMySQL(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.close() }()

	_, err = d.queryRows(ctx, "INSERT INTO weknora_ro_probe_does_not_exist (x) VALUES (1)", 0)
	if err == nil {
		t.Fatal("write statement was accepted by a read-only session")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "read only") {
		t.Fatalf("expected a read-only rejection, got: %v", err)
	}
}

// TestMySQLIntegrationRunQuery syncs the selected tables, then checks the query path
// against the live server: a read-only SELECT succeeds, the row cap truncates the
// result, and writes or references to tables that were not extracted are rejected.
func TestMySQLIntegrationRunQuery(t *testing.T) {
	ds := mysqlIntegrationConfig(t)
	if ds.Settings["database"] == "" {
		t.Skip("WEKNORA_EXTDB_TEST_MYSQL_DATABASE is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := NewMySQLConnector()
	if err := c.Validate(ctx, ds); err != nil {
		t.Fatalf("validate: %v", err)
	}
	schemas, err := c.ListResources(ctx, ds, "")
	if err != nil || len(schemas) != 1 {
		t.Fatalf("list schemas: %v (got %d)", err, len(schemas))
	}
	ds.ResourceIDs = []string{schemas[0].ExternalID}
	cur, err := c.FetchStream(ctx, ds, nil, &recHandler{})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	cfg, err := parseConfig(engineMySQL, ds)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeCursor(cur, engineMySQL, cfg.target())
	if err != nil {
		t.Fatal(err)
	}
	extracted := extractedTables(decoded)
	if len(extracted) == 0 {
		t.Skip("sync extracted no tables; nothing to query")
	}
	var table tableKey
	for k := range extracted {
		table = k
		break
	}
	quoted := "`" + table.Schema + "`.`" + table.Table + "`"
	limits := types.ExternalQueryLimits{MaxRows: 200, MaxBytes: 512 << 10, Timeout: time.Minute}

	t.Run("read-only select succeeds", func(t *testing.T) {
		res, err := c.RunQuery(ctx, ds, cur, "SELECT COUNT(*) AS n FROM "+quoted, limits)
		if err != nil {
			t.Fatalf("select rejected: %v", err)
		}
		if len(res.Rows) != 1 || len(res.Columns) != 1 {
			t.Fatalf("unexpected shape: columns=%v rows=%v", res.Columns, res.Rows)
		}
	})

	t.Run("row cap truncates", func(t *testing.T) {
		capped := limits
		capped.MaxRows = 3
		// Literal rows instead of a recursive CTE: the server may predate CTE support.
		res, err := c.RunQuery(ctx, ds, cur,
			"SELECT 1 AS x UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5",
			capped)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if len(res.Rows) != 3 || !res.Truncated {
			t.Fatalf("rows=%d truncated=%v, want 3 rows and truncated", len(res.Rows), res.Truncated)
		}
	})

	t.Run("write statements are rejected", func(t *testing.T) {
		for _, stmt := range []string{
			"DELETE FROM " + quoted,
			"UPDATE " + quoted + " SET x = 1",
			"SELECT * FROM " + quoted + " FOR UPDATE",
		} {
			if _, err := c.RunQuery(ctx, ds, cur, stmt, limits); err == nil {
				t.Fatalf("statement was accepted: %s", stmt)
			}
		}
	})

	t.Run("tables outside the extracted set are rejected", func(t *testing.T) {
		_, err := c.RunQuery(ctx, ds, cur, "SELECT * FROM `"+table.Schema+"`.`weknora_not_extracted_probe`", limits)
		if err == nil {
			t.Fatal("reference to a table that was not extracted was accepted")
		}
	})
}
