package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// sqlNodeNames returns the Name of every node of kind.
func sqlNodeNames(nodes []*graph.Node, kind graph.NodeKind) []string {
	var out []string
	for _, n := range nodesOfKind(nodes, kind) {
		out = append(out, n.Name)
	}
	return out
}

// nodeNamed returns the single node of kind with the given name.
func nodeNamed(t *testing.T, nodes []*graph.Node, kind graph.NodeKind, name string) *graph.Node {
	t.Helper()
	var found *graph.Node
	for _, n := range nodesOfKind(nodes, kind) {
		if n.Name == name {
			require.Nil(t, found, "duplicate %s node named %q", kind, name)
			found = n
		}
	}
	require.NotNil(t, found, "no %s node named %q in %v", kind, name, sqlNodeNames(nodes, kind))
	return found
}

// A schema-qualified CREATE TABLE names the node after the table, not
// after the schema. Reading the first identifier of the object_reference
// named `identity.kyc_submissions` "identity", so searching for the
// table returned nothing.
func TestSQLExtractor_SchemaQualifiedTableKeepsTableName(t *testing.T) {
	src := []byte(`CREATE TABLE identity.kyc_submissions (
    id UUID PRIMARY KEY,
    account_id UUID NOT NULL
);
`)
	result, err := NewSQLExtractor().Extract("db/migrations/000009_identity_kyc.up.sql", src)
	require.NoError(t, err)

	tbl := nodeNamed(t, result.Nodes, graph.KindType, "kyc_submissions")
	assert.Equal(t, "identity", tbl.Meta["schema"])
	assert.Equal(t, "identity.kyc_submissions", tbl.Meta["qualified_name"])
	assert.Equal(t, "db/migrations/000009_identity_kyc.up.sql::identity.kyc_submissions", tbl.ID)

	// Columns hang off the qualified table id.
	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 2)
	for _, e := range memberEdges {
		assert.Equal(t, tbl.ID, e.To)
	}
}

// Every schema-qualified table in a multi-statement migration survives.
// Keying the per-file dedupe on the (wrongly derived) schema name meant
// the first table in a schema shadowed every later one.
func TestSQLExtractor_MultiStatementMigrationKeepsEveryTable(t *testing.T) {
	src := []byte(`CREATE TABLE identity.accounts (
    id UUID PRIMARY KEY
);

CREATE TABLE identity.kyc_submissions (
    id UUID PRIMARY KEY
);

CREATE TABLE billing.balance_transactions (
    id BIGSERIAL PRIMARY KEY
);
`)
	result, err := NewSQLExtractor().Extract("db/migrations/000009_identity_kyc.up.sql", src)
	require.NoError(t, err)

	assert.ElementsMatch(t,
		[]string{"accounts", "kyc_submissions", "balance_transactions"},
		sqlNodeNames(result.Nodes, graph.KindType))
}

// The same table name under two schemas is two tables, not one.
func TestSQLExtractor_SameTableNameAcrossSchemas(t *testing.T) {
	src := []byte(`CREATE TABLE identity.accounts (id UUID PRIMARY KEY);
CREATE TABLE billing.accounts (id UUID PRIMARY KEY);
`)
	result, err := NewSQLExtractor().Extract("db/migrations/001_accounts.up.sql", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 2)
	var ids []string
	for _, n := range types {
		assert.Equal(t, "accounts", n.Name)
		ids = append(ids, n.ID)
	}
	assert.ElementsMatch(t, []string{
		"db/migrations/001_accounts.up.sql::identity.accounts",
		"db/migrations/001_accounts.up.sql::billing.accounts",
	}, ids)
}

// tree-sitter-sql has no grammar for dollar-quoted PL/pgSQL bodies, so a
// CREATE FUNCTION parses as an ERROR node that re-parents the rest of
// the file under itself. A top-level-only walk therefore stopped
// extracting at the first trigger function — the common shape of a
// Postgres migration.
func TestSQLExtractor_RecoversStatementsAfterDollarQuotedBody(t *testing.T) {
	src := []byte(`CREATE TABLE identity.accounts (
    id UUID PRIMARY KEY
);

CREATE OR REPLACE FUNCTION identity.set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER accounts_touch BEFORE UPDATE ON identity.accounts
    FOR EACH ROW EXECUTE FUNCTION identity.set_updated_at();

CREATE TABLE identity.kyc_submissions (
    id UUID PRIMARY KEY
);

CREATE TABLE billing.balance_transactions (
    id BIGSERIAL PRIMARY KEY
);
`)
	result, err := NewSQLExtractor().Extract("db/migrations/000009_identity_kyc.up.sql", src)
	require.NoError(t, err)

	// Tables declared after the function body are still extracted.
	assert.ElementsMatch(t,
		[]string{"accounts", "kyc_submissions", "balance_transactions"},
		sqlNodeNames(result.Nodes, graph.KindType))

	// So is the function the error node swallowed.
	fn := nodeNamed(t, result.Nodes, graph.KindFunction, "set_updated_at")
	assert.Equal(t, "identity", fn.Meta["schema"])
}

// CREATE INDEX takes its own name, not the name of the table it covers.
func TestSQLExtractor_IndexNamedAfterIndexNotTarget(t *testing.T) {
	src := []byte(`CREATE TABLE identity.kyc_submissions (id UUID PRIMARY KEY, account_id UUID);
CREATE INDEX idx_kyc_account ON identity.kyc_submissions (account_id);
CREATE UNIQUE INDEX idx_accounts_email ON accounts (email);
`)
	result, err := NewSQLExtractor().Extract("db/migrations/000009_identity_kyc.up.sql", src)
	require.NoError(t, err)

	idx := nodeNamed(t, result.Nodes, graph.KindVariable, "idx_kyc_account")
	assert.Equal(t, "index", idx.Meta["sql_type"])
	assert.Equal(t, "identity.kyc_submissions", idx.Meta["indexes"])

	unique := nodeNamed(t, result.Nodes, graph.KindVariable, "idx_accounts_email")
	assert.Equal(t, "accounts", unique.Meta["indexes"])
}

// Quoting is stripped per dotted segment, so a quoted name keeps its own
// dots instead of being split on the last one.
func TestSQLExtractor_QuotedSchemaQualifiedNames(t *testing.T) {
	src := []byte("CREATE TABLE \"billing\".\"balance_transactions\" (id BIGINT);\n" +
		"CREATE TABLE `app`.`events` (id INT);\n")
	result, err := NewSQLExtractor().Extract("db/migrations/002_billing.up.sql", src)
	require.NoError(t, err)

	bt := nodeNamed(t, result.Nodes, graph.KindType, "balance_transactions")
	assert.Equal(t, "billing", bt.Meta["schema"])

	ev := nodeNamed(t, result.Nodes, graph.KindType, "events")
	assert.Equal(t, "app", ev.Meta["schema"])
}

// Unqualified names keep the bare id they have always had — the schema
// fix must not renumber the single-schema files that already worked.
func TestSQLExtractor_UnqualifiedIDsUnchanged(t *testing.T) {
	src := []byte(`CREATE TABLE users (id INTEGER PRIMARY KEY);`)
	result, err := NewSQLExtractor().Extract("schema.sql", src)
	require.NoError(t, err)

	tbl := nodeNamed(t, result.Nodes, graph.KindType, "users")
	assert.Equal(t, "schema.sql::users", tbl.ID)
	assert.NotContains(t, tbl.Meta, "schema")
	assert.NotContains(t, tbl.Meta, "qualified_name")
}
