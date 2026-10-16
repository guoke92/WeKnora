package sqldb

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pganalyze/pg_query_go/v6"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"

	// The parser needs a value driver to build literals. The lightweight test driver
	// is enough here: the checks read names and structure, never evaluate expressions.
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

// Static checks for external_db_query. They run before any connection is opened and
// reject everything that is not one read-only SELECT over tables extracted from the
// data source. The read-only transaction the query runs in is the final guarantee;
// these checks keep the statement within the documented scope and give the agent a
// clear error to correct its SQL.

// tableKey identifies one table as (schema, table).
type tableKey struct{ Schema, Table string }

// unsafeMySQLFunctions are functions that block, take locks, or read server files.
var unsafeMySQLFunctions = map[string]bool{
	"sleep": true, "benchmark": true, "get_lock": true, "release_lock": true,
	"release_all_locks": true, "is_free_lock": true, "is_used_lock": true, "load_file": true,
}

// unsafePostgresFunctions are functions that write, lock, call out, or read server files.
var unsafePostgresFunctions = map[string]bool{
	"pg_sleep": true, "pg_sleep_for": true, "pg_sleep_until": true,
	"lo_import": true, "lo_export": true, "lo_unlink": true,
	"dblink": true, "dblink_exec": true, "dblink_connect": true,
	"pg_read_file": true, "pg_read_binary_file": true, "pg_ls_dir": true, "pg_stat_file": true,
	"pg_terminate_backend": true, "pg_cancel_backend": true, "pg_reload_conf": true,
	"pg_rotate_logfile": true, "set_config": true, "nextval": true, "setval": true,
	"pg_logical_emit_message": true,
}

// checkMySQLSelect parses one MySQL statement and returns the tables it references.
// Unqualified table names belong to defaultSchema, the database the connection uses.
func checkMySQLSelect(sqlText, defaultSchema string) ([]tableKey, error) {
	stmts, _, err := parser.New().ParseSQL(sqlText)
	if err != nil {
		return nil, fmt.Errorf("SQL 无法解析：%v", err)
	}
	if len(stmts) != 1 {
		return nil, errors.New("一次只能执行一条语句")
	}
	switch stmts[0].(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
	default:
		return nil, errors.New("只允许 SELECT 或 WITH ... SELECT 语句")
	}

	// Pass one collects common table expression names: they are not tables of the source.
	ctes := map[string]bool{}
	collect := &mysqlVisitor{defaultSchema: defaultSchema, ctes: ctes}
	stmts[0].Accept(collect)
	if collect.err != nil {
		return nil, collect.err
	}

	// Pass two checks the statement and collects the tables it reads.
	check := &mysqlVisitor{defaultSchema: defaultSchema, ctes: ctes, check: true,
		seen: map[tableKey]bool{}}
	stmts[0].Accept(check)
	if check.err != nil {
		return nil, check.err
	}
	return check.refs, nil
}

// mysqlVisitor walks a parsed statement. In collect mode it only records CTE names; in
// check mode it rejects locking reads, SELECT INTO, and unsafe functions, and records
// the tables referenced.
type mysqlVisitor struct {
	defaultSchema string
	ctes          map[string]bool
	check         bool
	seen          map[tableKey]bool
	refs          []tableKey
	err           error
}

func (v *mysqlVisitor) Enter(n ast.Node) (ast.Node, bool) {
	switch x := n.(type) {
	case *ast.CommonTableExpression:
		v.ctes[x.Name.L] = true
	case *ast.SelectStmt:
		if !v.check {
			break
		}
		if x.SelectIntoOpt != nil {
			v.err = errors.New("不允许 SELECT ... INTO")
		}
		if x.LockInfo != nil && x.LockInfo.LockType != ast.SelectLockNone {
			v.err = errors.New("不允许 FOR UPDATE / FOR SHARE 等锁定读")
		}
	case *ast.FuncCallExpr:
		if v.check && unsafeMySQLFunctions[x.FnName.L] {
			v.err = fmt.Errorf("不允许调用函数 %s", x.FnName.L)
		}
	case *ast.TableName:
		if !v.check {
			break
		}
		if x.Schema.O == "" && v.ctes[x.Name.L] {
			break
		}
		schema := x.Schema.O
		if schema == "" {
			schema = v.defaultSchema
		}
		key := tableKey{Schema: schema, Table: x.Name.O}
		if !v.seen[key] {
			v.seen[key] = true
			v.refs = append(v.refs, key)
		}
	}
	return n, v.err != nil
}

func (v *mysqlVisitor) Leave(n ast.Node) (ast.Node, bool) {
	return n, v.err == nil
}

// checkPostgresSelect parses one PostgreSQL statement and returns the tables it references.
// The statement is inspected through its JSON syntax tree, which keeps the walk generic
// over node kinds. Tables must be schema-qualified: an unqualified name would depend on
// the search_path of the session.
func checkPostgresSelect(sqlText string) ([]tableKey, error) {
	raw, err := pg_query.ParseToJSON(sqlText)
	if err != nil {
		return nil, fmt.Errorf("SQL 无法解析：%v", err)
	}
	var tree struct {
		Stmts []struct {
			Stmt map[string]json.RawMessage `json:"stmt"`
		} `json:"stmts"`
	}
	if err := json.Unmarshal([]byte(raw), &tree); err != nil {
		return nil, fmt.Errorf("SQL 语法树无法读取：%v", err)
	}
	if len(tree.Stmts) != 1 {
		return nil, errors.New("一次只能执行一条语句")
	}
	if _, ok := tree.Stmts[0].Stmt["SelectStmt"]; !ok {
		return nil, errors.New("只允许 SELECT 或 WITH ... SELECT 语句")
	}

	var root any
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return nil, err
	}

	// Pass one collects CTE names and rejects anything that writes, locks, or stores output.
	ctes := map[string]bool{}
	var walkErr error
	walkJSON(root, func(key string, value any) {
		if walkErr != nil {
			return
		}
		switch key {
		case "CommonTableExpr":
			if m, ok := value.(map[string]any); ok {
				if name, ok := m["ctename"].(string); ok {
					ctes[strings.ToLower(name)] = true
				}
			}
		case "IntoClause", "intoClause":
			if value != nil {
				walkErr = errors.New("不允许 SELECT ... INTO")
			}
		case "lockingClause":
			if list, ok := value.([]any); ok && len(list) > 0 {
				walkErr = errors.New("不允许 FOR UPDATE / FOR SHARE 等锁定读")
			}
		case "InsertStmt", "UpdateStmt", "DeleteStmt", "MergeStmt", "CopyStmt", "VariableSetStmt",
			"CallStmt", "DoStmt", "ExplainStmt", "VacuumStmt", "LockStmt", "TransactionStmt":
			walkErr = fmt.Errorf("不允许 %s", key)
		}
	})
	if walkErr != nil {
		return nil, walkErr
	}

	// Pass two rejects unsafe functions and collects the tables referenced.
	var refs []tableKey
	seen := map[tableKey]bool{}
	walkJSON(root, func(key string, value any) {
		if walkErr != nil {
			return
		}
		switch key {
		case "FuncCall":
			name := postgresFuncName(value)
			if unsafePostgresFunctions[name] || strings.HasPrefix(name, "pg_advisory_") {
				walkErr = fmt.Errorf("不允许调用函数 %s", name)
			}
		case "RangeVar":
			m, ok := value.(map[string]any)
			if !ok {
				return
			}
			rel, _ := m["relname"].(string)
			schema, _ := m["schemaname"].(string)
			if schema == "" {
				if ctes[strings.ToLower(rel)] {
					return
				}
				walkErr = fmt.Errorf("表 %s 必须使用 schema.table 形式引用", rel)
				return
			}
			key := tableKey{Schema: schema, Table: rel}
			if !seen[key] {
				seen[key] = true
				refs = append(refs, key)
			}
		}
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return refs, nil
}

// walkJSON calls visit for every object key in a decoded JSON tree, depth first.
func walkJSON(node any, visit func(key string, value any)) {
	switch v := node.(type) {
	case map[string]any:
		for key, value := range v {
			visit(key, value)
			walkJSON(value, visit)
		}
	case []any:
		for _, item := range v {
			walkJSON(item, visit)
		}
	}
}

// postgresFuncName returns the lower-cased, unqualified name of a FuncCall node.
func postgresFuncName(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	parts, _ := m["funcname"].([]any)
	if len(parts) == 0 {
		return ""
	}
	last, ok := parts[len(parts)-1].(map[string]any)
	if !ok {
		return ""
	}
	str, _ := last["String"].(map[string]any)
	sval, _ := str["sval"].(string)
	return strings.ToLower(sval)
}
