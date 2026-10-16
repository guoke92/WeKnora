package sqldb

import (
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/datasource"
)

// Resource identifiers are JSON arrays so that schema and table names may contain
// any character without ambiguity: ["shop"] is a schema, ["shop","orders"] a table.
// The same encoding is used for FetchedItem.ExternalID of table documents, which
// keeps the picker selection, the document identity and the cursor keys aligned.

// encodeSchemaID returns the resource identifier of a schema.
func encodeSchemaID(schema string) string {
	b, _ := json.Marshal([]string{schema})
	return string(b)
}

// encodeTableID returns the resource identifier (and document ExternalID) of a table.
func encodeTableID(schema, table string) string {
	b, _ := json.Marshal([]string{schema, table})
	return string(b)
}

// parsedID is a decoded resource identifier.
type parsedID struct {
	Schema  string
	Table   string
	IsTable bool
}

// parseResourceID decodes an identifier produced by encodeSchemaID / encodeTableID.
func parseResourceID(id string) (parsedID, error) {
	var parts []string
	if err := json.Unmarshal([]byte(id), &parts); err != nil {
		return parsedID{}, fmt.Errorf("%w: invalid resource id %q", datasource.ErrInvalidConfig, id)
	}
	switch {
	case len(parts) == 1 && parts[0] != "":
		return parsedID{Schema: parts[0]}, nil
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return parsedID{Schema: parts[0], Table: parts[1], IsTable: true}, nil
	default:
		return parsedID{}, fmt.Errorf("%w: invalid resource id %q", datasource.ErrInvalidConfig, id)
	}
}

// selection is the set of schemas and tables chosen in the resource picker.
// A selected schema covers every table in it; a selected table covers only itself.
type selection struct {
	schemas map[string]bool
	tables  map[string]map[string]bool // schema -> table -> true
}

func parseSelection(resourceIDs []string) (selection, error) {
	sel := selection{schemas: map[string]bool{}, tables: map[string]map[string]bool{}}
	for _, id := range resourceIDs {
		p, err := parseResourceID(id)
		if err != nil {
			return selection{}, err
		}
		if !p.IsTable {
			sel.schemas[p.Schema] = true
			continue
		}
		if sel.tables[p.Schema] == nil {
			sel.tables[p.Schema] = map[string]bool{}
		}
		sel.tables[p.Schema][p.Table] = true
	}
	return sel, nil
}

// schemaNames returns every schema the selection touches, sorted.
func (s selection) schemaNames() []string {
	seen := map[string]bool{}
	for name := range s.schemas {
		seen[name] = true
	}
	for name := range s.tables {
		seen[name] = true
	}
	return sortedKeys(seen)
}

// covers reports whether a table in the given schema is part of the selection.
func (s selection) covers(schema, table string) bool {
	return s.schemas[schema] || s.tables[schema][table]
}
