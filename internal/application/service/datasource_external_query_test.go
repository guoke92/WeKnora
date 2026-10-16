package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDataSourceRepo returns one fixed data source; every other repository method panics
// through the nil embedded interface, which keeps the test honest about what it touches.
type fakeDataSourceRepo struct {
	interfaces.DataSourceRepository
	ds *types.DataSource
}

func (r *fakeDataSourceRepo) FindByID(_ context.Context, _ string) (*types.DataSource, error) {
	return r.ds, nil
}

func TestRunExternalQueryRejectsNonDatabaseSource(t *testing.T) {
	svc := &DataSourceService{
		dsRepo: &fakeDataSourceRepo{ds: &types.DataSource{
			ID:              "ds-feishu",
			Type:            types.ConnectorTypeFeishu,
			KnowledgeBaseID: "kb-1",
		}},
	}

	res, err := svc.RunExternalQuery(context.Background(), "ds-feishu", "SELECT 1", types.ExternalQueryLimits{})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.True(t, errors.Is(err, datasource.ErrInvalidConfig), "error must wrap ErrInvalidConfig: %v", err)
}

func TestSQLDigestIdentifiesStatementWithoutTextOrSpaces(t *testing.T) {
	const stmt = "SELECT password FROM users WHERE email = 'secret@example.com'"
	digest := sqlDigest(stmt)

	assert.Len(t, digest, 16, "digest is 8 bytes in hex")
	assert.Equal(t, digest, sqlDigest(stmt), "digest must be deterministic")
	assert.NotEqual(t, digest, sqlDigest(stmt+" "), "digest must change with the statement")
	assert.False(t, strings.Contains(digest, "secret"), "digest must not expose statement text")
}
