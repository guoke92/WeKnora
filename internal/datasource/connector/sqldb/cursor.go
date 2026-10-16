package sqldb

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

// cursorSchemaVersion is bumped whenever the document format or the cursor layout
// changes, so that a stale cursor forces a full re-render instead of silently
// reusing fingerprints computed by an older renderer.
const cursorSchemaVersion = 1

// cursor is the resumable sync state. Tables maps each table's ExternalID to the
// SHA-256 of its last emitted document. A table whose fingerprint is unchanged is not
// emitted again; a table missing from the picker or the database is deleted.
type cursor struct {
	SchemaVersion int               `json:"schema_version"`
	Engine        string            `json:"engine"`
	Target        string            `json:"target"`
	Tables        map[string]string `json:"tables"`
	// Analysis caches model results per table. An entry is reused while its key (the
	// exact prompt, model and language) is unchanged, so unchanged tables cost no tokens.
	Analysis map[string]cachedSemantics `json:"analysis,omitempty"`
}

// decodeCursor accepts a nil or empty cursor as fresh state. A cursor synced against a
// different database is rejected: silently continuing would delete the previous
// database's documents.
func decodeCursor(old *types.SyncCursor, engine, target string) (cursor, error) {
	fresh := cursor{SchemaVersion: cursorSchemaVersion, Engine: engine, Target: target,
		Tables: map[string]string{}, Analysis: map[string]cachedSemantics{}}
	if old == nil || len(old.ConnectorCursor) == 0 {
		return fresh, nil
	}
	raw, err := json.Marshal(old.ConnectorCursor)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: invalid %s cursor: %v", datasource.ErrInvalidConfig, engine, err)
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return cursor{}, fmt.Errorf("%w: invalid %s cursor: %v", datasource.ErrInvalidConfig, engine, err)
	}
	if c.SchemaVersion != cursorSchemaVersion {
		// Renderer or layout changed: start over so every document is rewritten once.
		return fresh, nil
	}
	if c.Target != "" && c.Target != target {
		return cursor{}, fmt.Errorf(
			"%w: this data source already synced %s; restore that database or create a new data source instead of switching to %s",
			datasource.ErrInvalidConfig, c.Target, target)
	}
	c.Engine = engine
	c.Target = target
	if c.Tables == nil {
		c.Tables = map[string]string{}
	}
	if c.Analysis == nil {
		c.Analysis = map[string]cachedSemantics{}
	}
	return c, nil
}

// toSyncCursor returns a snapshot independent of the working maps (JSON round trip),
// as required by the StreamHandler.Checkpoint contract.
func (c *cursor) toSyncCursor() *types.SyncCursor {
	raw, _ := json.Marshal(c)
	fields := map[string]interface{}{}
	_ = json.Unmarshal(raw, &fields)
	return &types.SyncCursor{LastSyncTime: time.Now().UTC(), ConnectorCursor: fields}
}
