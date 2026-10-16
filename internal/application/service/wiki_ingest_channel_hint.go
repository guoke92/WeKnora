package service

import "github.com/Tencent/WeKnora/internal/types"

// dbTableCompileHint tells the wiki compiler what a table document contains, so the
// page it writes follows the table's structure instead of a generic summary. The
// document itself is produced by the sqldb connector (structure, keys, relations,
// profile); the hint only shapes how the compiler reads it.
const dbTableCompileHint = `[Source note] This document describes one table of an external relational database (structure, keys, declared and inferred relations, column profile). Compile it as a table page:
- State the table's business purpose from its name, comments and column names only. If the evidence is thin, say so.
- List the primary key and the key columns.
- Keep declared foreign keys separate from inferred relations. Quote the evidence figures given for inferred relations; relations described as value overlap have no direction.
- Explain enumeration-like columns only from their comments or value statistics. Do not invent the meaning of codes.
- Sections titled "语义解读" are model-assisted readings of the structure, not facts from the database.`

// compileHintForChannel returns extra instructions for the wiki compiler when a source
// document comes from an external database, or an empty string for other sources.
func compileHintForChannel(channel string) string {
	switch channel {
	case types.ChannelMySQL, types.ChannelPostgres:
		return dbTableCompileHint
	}
	return ""
}
