package sqldb

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	engineMySQL    = types.ConnectorTypeMySQL
	enginePostgres = types.ConnectorTypePostgres

	tlsDisable    = "disable"
	tlsRequire    = "require"
	tlsVerifyFull = "verify-full"

	defaultTopK         = 10
	maxTopK             = 50
	defaultSampleRows   = 10000
	maxSampleRows       = 100000
	defaultMaxTables    = 200
	maxMaxTables        = 5000
	defaultQueryTimeout = 30 * time.Second
	maxQueryTimeout     = 10 * time.Minute
	maxProfileColumns   = 200

	defaultLLMTokenBudget = 500000
	maxLLMTokenBudget     = 5000000
)

// config is the validated, typed view of a MySQL/PostgreSQL data source.
// Non-secret knobs live in Settings; user and password are credentials.
type config struct {
	engine string
	host   string
	port   int
	// database is the database (MySQL schema / PostgreSQL database) to read.
	database string
	user     string
	password string
	tls      string

	profileEnabled bool // column-level profiling (L2)
	profileValues  bool // include value-level features (min/max, TopK); sensitive columns are never shown
	topK           int
	sampleRows     int
	maskedColumns  map[string]bool // lower-cased names forced to the sensitive policy
	maxTables      int
	queryTimeout   time.Duration

	llmAnalysis    bool // model-assisted table analysis (L4), used when the knowledge base has a chat model
	llmTokenBudget int  // model tokens one sync may spend on the analysis step
}

// target identifies the database a cursor is bound to. A cursor synced against one
// database must not silently continue against another.
func (c config) target() string {
	return fmt.Sprintf("%s://%s:%d/%s", c.engine, strings.ToLower(c.host), c.port, c.database)
}

// parseConfig validates the data source configuration for the given engine.
func parseConfig(engine string, ds *types.DataSourceConfig) (config, error) {
	if ds == nil {
		return config{}, datasource.ErrInvalidConfig
	}
	s := ds.Settings
	c := config{engine: engine}

	c.host = settingString(s, "host")
	if c.host == "" {
		return config{}, fmt.Errorf("%w: host is required", datasource.ErrInvalidConfig)
	}
	c.database = settingString(s, "database")
	if c.database == "" {
		return config{}, fmt.Errorf("%w: database is required", datasource.ErrInvalidConfig)
	}
	defaultPort := 3306
	if engine == enginePostgres {
		defaultPort = 5432
	}
	c.port = settingInt(s, "port", defaultPort, 1, 65535)

	c.user = credentialString(ds, "user")
	c.password = credentialRaw(ds, "password")
	if c.user == "" {
		return config{}, fmt.Errorf("%w: user is required", datasource.ErrInvalidConfig)
	}

	c.tls = strings.ToLower(settingString(s, "tls_mode"))
	if c.tls == "" {
		c.tls = tlsRequire
	}
	switch c.tls {
	case tlsDisable, tlsRequire, tlsVerifyFull:
	default:
		return config{}, fmt.Errorf("%w: tls_mode must be disable, require or verify-full", datasource.ErrInvalidConfig)
	}

	c.profileEnabled = settingBool(s, "profile_enabled", true)
	c.profileValues = settingBool(s, "profile_values", true)
	c.topK = settingInt(s, "topk", defaultTopK, 1, maxTopK)
	c.sampleRows = settingInt(s, "profile_sample_rows", defaultSampleRows, 100, maxSampleRows)
	c.maxTables = settingInt(s, "max_tables", defaultMaxTables, 1, maxMaxTables)
	c.queryTimeout = time.Duration(settingInt(s, "query_timeout_seconds", 30, 1,
		int(maxQueryTimeout/time.Second))) * time.Second
	c.llmAnalysis = settingBool(s, "llm_analysis", true)
	c.llmTokenBudget = settingInt(s, "llm_token_budget", defaultLLMTokenBudget, 0, maxLLMTokenBudget)

	c.maskedColumns = map[string]bool{}
	for _, name := range settingStrings(s, "masked_columns") {
		c.maskedColumns[strings.ToLower(name)] = true
	}
	return c, nil
}

func settingString(s map[string]interface{}, key string) string {
	if s == nil {
		return ""
	}
	if v, ok := s[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func settingInt(s map[string]interface{}, key string, def, lo, hi int) int {
	if s == nil {
		return def
	}
	var n int
	switch v := s[key].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case int64:
		n = int(v)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return def
		}
		n = parsed
	default:
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func settingBool(s map[string]interface{}, key string, def bool) bool {
	if s == nil {
		return def
	}
	switch v := s[key].(type) {
	case bool:
		return v
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

// settingStrings accepts either a JSON array or a comma/newline separated string.
func settingStrings(s map[string]interface{}, key string) []string {
	if s == nil {
		return nil
	}
	var out []string
	switch v := s[key].(type) {
	case []interface{}:
		for _, item := range v {
			if str, ok := item.(string); ok && strings.TrimSpace(str) != "" {
				out = append(out, strings.TrimSpace(str))
			}
		}
	case []string:
		for _, str := range v {
			if strings.TrimSpace(str) != "" {
				out = append(out, strings.TrimSpace(str))
			}
		}
	case string:
		for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' }) {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func credentialString(ds *types.DataSourceConfig, key string) string {
	return strings.TrimSpace(credentialRaw(ds, key))
}

func credentialRaw(ds *types.DataSourceConfig, key string) string {
	if ds == nil || ds.Credentials == nil {
		return ""
	}
	if v, ok := ds.Credentials[key].(string); ok {
		return v
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
