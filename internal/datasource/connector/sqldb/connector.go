// Package sqldb turns a MySQL or PostgreSQL database into knowledge documents.
//
// P1a covers the structural layer (L1) and the column profile (L2): one Markdown
// document per table, with columns, keys, indexes, declared foreign keys and per-column
// features from a bounded sample. Every query runs inside a READ ONLY transaction (and,
// for PostgreSQL, a session with default_transaction_read_only=on), and every connection
// dials through the SSRF-safe dialer.
package sqldb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// checkpointEvery bounds how much work is repeated after a crash: the cursor is
// persisted after this many changed tables.
const checkpointEvery = 20

var (
	_ datasource.Connector              = (*Connector)(nil)
	_ datasource.StreamingConnector     = (*Connector)(nil)
	_ datasource.FullStreamingConnector = (*Connector)(nil)
)

// Connector serves one engine. Use NewMySQLConnector or NewPostgresConnector.
type Connector struct {
	engine string
	open   opener
}

// NewMySQLConnector creates the MySQL connector registered in the container.
func NewMySQLConnector() *Connector {
	return &Connector{engine: engineMySQL, open: openMySQL}
}

// NewPostgresConnector creates the PostgreSQL connector registered in the container.
func NewPostgresConnector() *Connector {
	return &Connector{engine: enginePostgres, open: openPostgres}
}

// Type returns the connector key stored on data sources.
func (c *Connector) Type() string { return c.engine }

// Validate checks the settings and opens a read-only session.
func (c *Connector) Validate(ctx context.Context, ds *types.DataSourceConfig) error {
	cfg, err := parseConfig(c.engine, ds)
	if err != nil {
		return err
	}
	if _, err := parseSelection(ds.ResourceIDs); err != nil {
		return err
	}
	d, err := c.openDialect(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = d.close() }()
	if err := d.ping(ctx); err != nil {
		return fmt.Errorf("%w: cannot read database %s on %s: %v",
			datasource.ErrInvalidConfig, cfg.database, cfg.target(), err)
	}
	return nil
}

// ListResources lists schemas at the top level and tables beneath a schema.
func (c *Connector) ListResources(
	ctx context.Context, ds *types.DataSourceConfig, parentID string,
) ([]types.Resource, error) {
	cfg, err := parseConfig(c.engine, ds)
	if err != nil {
		return nil, err
	}
	d, err := c.openDialect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.close() }()

	if parentID == "" {
		schemas, err := d.listSchemas(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: list schemas: %v", datasource.ErrFetchFailed, err)
		}
		out := make([]types.Resource, 0, len(schemas))
		for _, s := range schemas {
			out = append(out, types.Resource{
				ExternalID:  encodeSchemaID(s),
				Name:        s,
				Type:        "schema",
				HasChildren: true,
			})
		}
		return out, nil
	}

	p, err := parseResourceID(parentID)
	if err != nil {
		return nil, err
	}
	if p.IsTable {
		return nil, nil
	}
	tables, err := d.listTables(ctx, p.Schema)
	if err != nil {
		return nil, fmt.Errorf("%w: list tables of %s: %v", datasource.ErrFetchFailed, p.Schema, err)
	}
	out := make([]types.Resource, 0, len(tables))
	for _, t := range tables {
		out = append(out, types.Resource{
			ExternalID:  encodeTableID(p.Schema, t.Name),
			Name:        t.Name,
			Type:        "table",
			Description: t.Comment,
			ParentID:    parentID,
			Metadata:    map[string]interface{}{"row_estimate": t.RowEstimate},
		})
	}
	return out, nil
}

// ResolveResourceAncestors returns the schema of every selected table so the picker can
// reveal a pre-existing selection without a database round trip.
func (c *Connector) ResolveResourceAncestors(
	_ context.Context, _ *types.DataSourceConfig, resourceIDs []string,
) ([]string, error) {
	seen := map[string]bool{}
	for _, id := range resourceIDs {
		p, err := parseResourceID(id)
		if err != nil {
			return nil, err
		}
		if p.IsTable {
			seen[encodeSchemaID(p.Schema)] = true
		}
	}
	return sortedKeys(seen), nil
}

// FetchAll collects a fresh sync of the selected resources.
func (c *Connector) FetchAll(
	ctx context.Context, ds *types.DataSourceConfig, resourceIDs []string,
) ([]types.FetchedItem, error) {
	if ds == nil {
		return nil, datasource.ErrInvalidConfig
	}
	dsCopy := *ds
	dsCopy.ResourceIDs = resourceIDs
	items, _, err := c.FetchIncremental(ctx, &dsCopy, nil)
	return items, err
}

// FetchIncremental collects one streaming run into memory.
func (c *Connector) FetchIncremental(
	ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor,
) ([]types.FetchedItem, *types.SyncCursor, error) {
	h := &collector{}
	next, err := c.FetchStream(ctx, ds, old, h)
	return h.items, next, err
}

// FetchStream emits tables whose document changed since the cursor and deletes tables
// that left the selection or the database.
func (c *Connector) FetchStream(
	ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor, h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	return c.runSync(ctx, ds, old, h, false)
}

// FetchFullStream re-emits every table and keeps the previous cursor only as the
// baseline for deletion reconciliation.
func (c *Connector) FetchFullStream(
	ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor, h datasource.StreamHandler,
) (*types.SyncCursor, error) {
	return c.runSync(ctx, ds, old, h, true)
}

func (c *Connector) openDialect(ctx context.Context, cfg config) (dialect, error) {
	d, err := c.open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open %s connection to %s: %v",
			datasource.ErrInvalidConfig, c.engine, cfg.target(), err)
	}
	return d, nil
}

func (c *Connector) runSync(
	ctx context.Context, ds *types.DataSourceConfig, old *types.SyncCursor,
	h datasource.StreamHandler, forceFull bool,
) (*types.SyncCursor, error) {
	cfg, err := parseConfig(c.engine, ds)
	if err != nil {
		return nil, err
	}
	sel, err := parseSelection(ds.ResourceIDs)
	if err != nil {
		return nil, err
	}
	if len(sel.schemas) == 0 && len(sel.tables) == 0 {
		return nil, fmt.Errorf("%w: select at least one schema or table", datasource.ErrInvalidConfig)
	}
	cur, err := decodeCursor(old, c.engine, cfg.target())
	if err != nil {
		return nil, err
	}
	d, err := c.openDialect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.close() }()

	r := &syncRun{cfg: cfg, d: d, sel: sel, cur: cur, h: h, forceFull: forceFull, seen: map[string]bool{}}
	return r.run(ctx)
}

// collector adapts the streaming engine to the collecting Connector methods.
type collector struct{ items []types.FetchedItem }

func (h *collector) Emit(_ context.Context, item types.FetchedItem) error {
	h.items = append(h.items, item)
	return nil
}

func (*collector) Checkpoint(context.Context, *types.SyncCursor) error { return nil }

// target is one table scheduled for this run.
type target struct {
	schema string
	ref    tableRef
}

// syncRun is the state of one sync pass.
type syncRun struct {
	cfg       config
	d         dialect
	sel       selection
	cur       cursor
	h         datasource.StreamHandler
	forceFull bool

	seen    map[string]bool // resource ids that exist in the selection this run
	unsaved int             // changed tables since the last checkpoint
}

func (r *syncRun) run(ctx context.Context) (*types.SyncCursor, error) {
	targets, err := r.collectTargets(ctx)
	if err != nil {
		return nil, err
	}

	if len(targets) > r.cfg.maxTables {
		skipped := targets[r.cfg.maxTables:]
		targets = targets[:r.cfg.maxTables]
		// Skipped tables are still "seen": their previous documents are kept rather than
		// deleted just because the run stopped early.
		for _, t := range skipped {
			r.seen[encodeTableID(t.schema, t.ref.Name)] = true
		}
		if err := r.emitError(ctx, "", fmt.Sprintf("%d tables skipped (max_tables=%d)", len(skipped), r.cfg.maxTables),
			"max_tables_exceeded", "Table limit reached; raise max_tables or narrow the selection"); err != nil {
			return nil, err
		}
	}

	// Phase 1 reads every table. Relations need the samples of all tables, so documents
	// are rendered only once the whole selection has been read.
	works := make([]tableWork, 0, len(targets))
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %v", datasource.ErrSyncCanceled, err)
		}
		id := encodeTableID(t.schema, t.ref.Name)
		r.seen[id] = true

		w, err := r.gather(ctx, t)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("%w: %v", datasource.ErrSyncCanceled, ctxErr)
			}
			logger.Warnf(ctx, "[sqldb] describe %s.%s failed: %v", t.schema, t.ref.Name, err)
			// The previous fingerprint is kept, so the table is retried on the next run.
			if err := r.emitError(ctx, id, t.schema+"."+t.ref.Name,
				"describe_failed", "Failed to read table structure; see server logs"); err != nil {
				return nil, err
			}
			continue
		}
		works = append(works, w)
	}

	// Phase 2 discovers relations among the tables read in phase 1 (L3), then the model
	// step (L4) explains the tables and judges those relations.
	rels := r.discover(ctx, works)
	sems, err := r.analyze(ctx, works, rels)
	if err != nil {
		return nil, err
	}
	for _, w := range works {
		applySemantics(rels[w.id], semanticResult(sems[w.id]))
	}

	// Phase 3 renders each document and emits only those whose fingerprint changed.
	for _, w := range works {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %v", datasource.ErrSyncCanceled, err)
		}
		doc, fp := r.document(w, rels[w.id], sems[w.id])
		if !r.forceFull && r.cur.Tables[w.id] == fp {
			continue
		}
		if err := r.h.Emit(ctx, r.documentItem(w.t, w.id, doc)); err != nil {
			return nil, err
		}
		r.cur.Tables[w.id] = fp
		r.unsaved++
		if r.unsaved >= checkpointEvery {
			if err := r.checkpoint(ctx); err != nil {
				return nil, err
			}
		}
	}

	var missing []string
	for id := range r.cur.Tables {
		if !r.seen[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		if err := r.checkpoint(ctx); err != nil {
			return nil, err
		}
	}
	for _, id := range missing {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %v", datasource.ErrSyncCanceled, err)
		}
		p, err := parseResourceID(id)
		if err != nil {
			delete(r.cur.Tables, id)
			continue
		}
		if err := r.h.Emit(ctx, r.deletedItem(p, id)); err != nil {
			return nil, err
		}
		delete(r.cur.Tables, id)
		delete(r.cur.Analysis, id)
	}

	if err := r.checkpoint(ctx); err != nil {
		return nil, err
	}
	return r.cur.toSyncCursor(), nil
}

// collectTargets lists the tables of every touched schema and keeps those in the
// selection. A selected table that no longer exists is simply absent and gets deleted.
func (r *syncRun) collectTargets(ctx context.Context) ([]target, error) {
	var out []target
	for _, schema := range r.sel.schemaNames() {
		refs, err := r.d.listTables(ctx, schema)
		if err != nil {
			return nil, fmt.Errorf("%w: list tables of %s: %v", datasource.ErrFetchFailed, schema, err)
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
		for _, ref := range refs {
			if r.sel.covers(schema, ref.Name) {
				out = append(out, target{schema: schema, ref: ref})
			}
		}
	}
	return out, nil
}

// tableWork is one table read from the database, waiting for relations and rendering.
type tableWork struct {
	t    target
	id   string
	meta *tableMeta
	prof *tableProfile // nil when profiling is disabled or failed
	note string        // explains a failed profile
}

// gather reads the structure and, when enabled, the sample of one table. A failing
// profile degrades to a structure-only document with a note; a failing describe is an error.
func (r *syncRun) gather(ctx context.Context, t target) (tableWork, error) {
	dctx, cancel := context.WithTimeout(ctx, r.cfg.queryTimeout*3)
	defer cancel()
	meta, err := r.d.describe(dctx, t.schema, t.ref.Name)
	if err != nil {
		return tableWork{}, err
	}
	w := tableWork{t: t, id: encodeTableID(t.schema, t.ref.Name), meta: meta}
	if r.cfg.profileEnabled {
		pctx, pcancel := context.WithTimeout(ctx, r.cfg.queryTimeout*3)
		w.prof, err = profileTable(pctx, r.d, meta, r.cfg)
		pcancel()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return tableWork{}, ctxErr
			}
			logger.Warnf(ctx, "[sqldb] profile %s.%s failed: %v", t.schema, t.ref.Name, err)
			w.prof = nil
			w.note = "采样失败，本次未采集列画像（详见服务端日志）"
		}
	}
	return w, nil
}

// discover finds inferred relations among the profiled tables. It returns nil when
// profiling is disabled, which leaves the relation section out of the documents.
func (r *syncRun) discover(ctx context.Context, works []tableWork) map[string]*tableRelations {
	if !r.cfg.profileEnabled {
		return nil
	}
	var tables []relTable
	var ids []string
	for _, w := range works {
		if w.prof == nil {
			continue
		}
		tables = append(tables, relTable{
			ID: w.id, Schema: w.t.schema, Name: w.t.ref.Name, Meta: w.meta, Keys: w.prof.keys,
		})
		ids = append(ids, w.id)
	}
	findings, skipped := discoverRelations(tables)
	if skipped {
		logger.Warnf(ctx, "[sqldb] relation discovery skipped: more than %d key columns", relMaxKeyColumns)
	}
	return relationsByTable(findings, ids, skipped)
}

// document renders one table and fingerprints the result. The fingerprint covers the
// relations too, so a change in another table re-emits the documents it affects.
func (r *syncRun) document(w tableWork, rel *tableRelations, sem *semanticState) (string, string) {
	doc := renderTable(r.cfg, w.meta, w.prof, w.note, rel, sem)
	sum := sha256.Sum256([]byte(doc))
	return doc, hex.EncodeToString(sum[:])
}

func (r *syncRun) checkpoint(ctx context.Context) error {
	r.unsaved = 0
	return r.h.Checkpoint(ctx, r.cur.toSyncCursor())
}

// documentItem is the FetchedItem for a table document. Source metadata identifies the
// database so ingestion can route the document into the knowledge base like any other
// connector output.
func (r *syncRun) documentItem(t target, id, doc string) types.FetchedItem {
	channel := types.ChannelMySQL
	if r.cfg.engine == enginePostgres {
		channel = types.ChannelPostgres
	}
	name := t.schema + "." + t.ref.Name
	return types.FetchedItem{
		ExternalID:       id,
		Title:            name,
		Content:          []byte(doc),
		ContentType:      "text/markdown",
		FileName:         sanitizeFileName(name) + ".md",
		SourceResourceID: encodeSchemaID(t.schema),
		Metadata: map[string]string{
			"channel":     channel,
			"source_type": r.cfg.engine,
			"db_schema":   t.schema,
			"db_table":    t.ref.Name,
		},
	}
}

func (r *syncRun) deletedItem(p parsedID, id string) types.FetchedItem {
	channel := types.ChannelMySQL
	if r.cfg.engine == enginePostgres {
		channel = types.ChannelPostgres
	}
	return types.FetchedItem{
		ExternalID:       id,
		Title:            p.Schema + "." + p.Table,
		IsDeleted:        true,
		SourceResourceID: encodeSchemaID(p.Schema),
		Metadata:         map[string]string{"channel": channel, "source_type": r.cfg.engine},
	}
}

// emitError reports a per-table or per-run failure as an error item, which the sync
// log counts as a partial failure. Messages are fixed text: driver errors may carry
// connection details and stay in the server log.
func (r *syncRun) emitError(ctx context.Context, id, title, code, message string) error {
	return r.h.Emit(ctx, types.FetchedItem{
		ExternalID: id,
		Title:      title,
		Metadata: map[string]string{
			"error":             message,
			"error_reason_code": code,
			"error_reason":      message,
			"channel":           channelOf(r.cfg.engine),
			"source_type":       r.cfg.engine,
		},
	})
}

func channelOf(engine string) string {
	if engine == enginePostgres {
		return types.ChannelPostgres
	}
	return types.ChannelMySQL
}

// sanitizeFileName keeps the name readable and safe for the file store.
func sanitizeFileName(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", " ", "_", "\n", "_", "\r", "_")
	out := replacer.Replace(name)
	if len(out) > 180 {
		out = out[:180]
	}
	return out
}
