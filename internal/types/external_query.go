package types

import "time"

// ExternalQueryLimits bounds one query against an external database. A query that
// reaches a limit is stopped and reported as truncated; the SQL is never rewritten.
type ExternalQueryLimits struct {
	MaxRows  int
	MaxBytes int
	Timeout  time.Duration
}

// ExternalQueryResult is the bounded result of one checked query. Every value is
// rendered as text, NULL as "NULL".
type ExternalQueryResult struct {
	Columns   []string
	Rows      [][]string
	Bytes     int
	Truncated bool
	Duration  time.Duration
}
