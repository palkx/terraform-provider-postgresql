package postgresql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lib/pq"
)

// PostgreSQL stores all per-database settings for a given (database)
// pair in a single row of pg_db_role_setting (unique on (setdatabase, setrole)).
// Concurrent ALTER ROLE … IN DATABASE … SET statements that target the same
// pair race on this unique constraint, so we serialize them with an advisory
// lock keyed by (database).
const acquireDatabaseSettingLockSQL = `SELECT pg_advisory_xact_lock(hashtext($1))`

const (
	databaseSettingDatabaseAttr  = "database"
	databaseSettingParameterAttr = "parameter"
	databaseSettingValueAttr     = "value"
)

func resourcePostgreSQLDatabaseSetting() *schema.Resource {
	return &schema.Resource{
		Create: PGResourceFunc(resourcePostgreSQLDatabaseSettingCreate),
		Read:   PGResourceFunc(resourcePostgreSQLDatabaseSettingRead),
		Update: PGResourceFunc(resourcePostgreSQLDatabaseSettingUpdate),
		Delete: PGResourceFunc(resourcePostgreSQLDatabaseSettingDelete),
		Importer: &schema.ResourceImporter{
			StateContext: resourcePostgreSQLDatabaseSettingImport,
		},

		Schema: map[string]*schema.Schema{
			databaseSettingDatabaseAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The database in which the setting applies (IN DATABASE <database>).",
			},
			databaseSettingParameterAttr: {
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
				Description: "The configuration parameter to set (e.g. role, search_path, statement_timeout).",
			},
			databaseSettingValueAttr: {
				Type:        schema.TypeString,
				Required:    true,
				Description: "The value to assign to the parameter for this database.",
			},
		},
	}
}

func resourcePostgreSQLDatabaseSettingCreate(db *DBConnection, d *schema.ResourceData) error {
	if err := applyDatabaseSetting(db, d); err != nil {
		return err
	}
	d.SetId(generateDatabaseSettingID(d))
	return resourcePostgreSQLDatabaseSettingReadImpl(db, d)
}

func resourcePostgreSQLDatabaseSettingUpdate(db *DBConnection, d *schema.ResourceData) error {
	if err := applyDatabaseSetting(db, d); err != nil {
		return err
	}
	return resourcePostgreSQLDatabaseSettingReadImpl(db, d)
}

func resourcePostgreSQLDatabaseSettingRead(db *DBConnection, d *schema.ResourceData) error {
	return resourcePostgreSQLDatabaseSettingReadImpl(db, d)
}

func resourcePostgreSQLDatabaseSettingDelete(db *DBConnection, d *schema.ResourceData) error {
	database := d.Get(databaseSettingDatabaseAttr).(string)
	parameter := d.Get(databaseSettingParameterAttr).(string)

	// If database has been dropped externally, treat the setting as
	// already gone — RESET would otherwise fail with "database does not exist".
	exists, err := databaseExist(db, database)
	if err != nil {
		return err
	}
	if !exists {
		log.Printf("[WARN] PostgreSQL database %q no longer exists; skipping RESET %s", database, parameter)
		d.SetId("")
		return nil
	}

	txn, err := startTransaction(db.client, "")
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(acquireDatabaseSettingLockSQL, database); err != nil {
		return fmt.Errorf("could not acquire database-setting lock: %w", err)
	}

	stmt := fmt.Sprintf(
		"ALTER DATABASE %s RESET %s",
		pq.QuoteIdentifier(database),
		pq.QuoteIdentifier(parameter),
	)
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("could not reset %q in database %q: %w", parameter, database, err)
	}
	if err := txn.Commit(); err != nil {
		return fmt.Errorf("could not commit database-setting reset: %w", err)
	}
	d.SetId("")
	return nil
}

func applyDatabaseSetting(db *DBConnection, d *schema.ResourceData) error {
	database := d.Get(databaseSettingDatabaseAttr).(string)
	parameter := d.Get(databaseSettingParameterAttr).(string)
	value := d.Get(databaseSettingValueAttr).(string)

	txn, err := startTransaction(db.client, "")
	if err != nil {
		return err
	}
	defer deferredRollback(txn)

	if _, err := txn.Exec(acquireDatabaseSettingLockSQL, database); err != nil {
		return fmt.Errorf("could not acquire database-setting lock: %w", err)
	}

	stmt := fmt.Sprintf(
		"ALTER DATABASE %s SET %s = %s",
		pq.QuoteIdentifier(database),
		pq.QuoteIdentifier(parameter),
		pq.QuoteLiteral(value),
	)
	if _, err := txn.Exec(stmt); err != nil {
		return fmt.Errorf("could not set %q in database %q: %w", parameter, database, err)
	}
	if err := txn.Commit(); err != nil {
		return fmt.Errorf("could not commit database-setting update: %w", err)
	}
	return nil
}

func resourcePostgreSQLDatabaseSettingReadImpl(db *DBConnection, d *schema.ResourceData) error {
	database := d.Get(databaseSettingDatabaseAttr).(string)
	parameter := d.Get(databaseSettingParameterAttr).(string)

	const query = `
SELECT s.setconfig
FROM pg_db_role_setting s
JOIN pg_database dbs ON dbs.oid = s.setdatabase
WHERE dbs.datname = $1
`
	var setconfig []string
	err := db.QueryRow(query, database).Scan(pq.Array(&setconfig))
	switch {
	case err == sql.ErrNoRows:
		log.Printf("[WARN] no pg_db_role_setting row for database %q", database)
		d.SetId("")
		return nil
	case err != nil:
		return fmt.Errorf("error reading role database setting: %w", err)
	}

	value, found := findSetconfigDBValue(setconfig, parameter)
	if !found {
		log.Printf("[WARN] parameter %q not found in setconfig for database %q", parameter, database)
		d.SetId("")
		return nil
	}

	d.Set(databaseSettingDatabaseAttr, database)
	d.Set(databaseSettingParameterAttr, parameter)
	d.Set(databaseSettingValueAttr, value)
	d.SetId(generateDatabaseSettingID(d))
	return nil
}

// findSetconfigDBValue searches a pg_db_role_setting.setconfig array (each
// element formatted as "key=value") for the requested parameter and returns
// the unquoted value. Parameter name comparison is case-insensitive because
// PostgreSQL canonicalizes GUC names to lowercase in the catalog.
func findSetconfigDBValue(setconfig []string, parameter string) (string, bool) {
	target := strings.ToLower(parameter)
	for _, entry := range setconfig {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if strings.ToLower(k) == target {
			return stripSetconfigDBQuotes(v), true
		}
	}
	return "", false
}

// stripSetconfigDBQuotes removes the surrounding double quotes that PostgreSQL
// adds in setconfig when a value contains characters that need quoting
// (e.g. commas in `search_path="app, public"`). Inside the wrapped form
// PostgreSQL escapes embedded `"` by doubling it (`""`), so we undo that.
// Backslash escapes are NOT used at this layer — those belong to the array
// I/O layer and have already been decoded by pq.Array before we see the
// element.
func stripSetconfigDBQuotes(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
		v = strings.ReplaceAll(v, `""`, `"`)
	}
	return v
}

func databaseExist(db *DBConnection, database string) (bool, error) {
	var ok bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`,
		database,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("could not check existence of database %q: %w", database, err)
	}
	return ok, nil
}

// generateDatabaseSettingID encodes the (database, parameter)
// triple into a Terraform resource ID. The components are joined with ':',
// with literal ':' and '\' inside any component backslash-escaped so the ID
// round-trips for any valid PostgreSQL identifier (which can contain ':'
// when quoted). Components without these characters round-trip without any
// visible escaping.
func generateDatabaseSettingID(d *schema.ResourceData) string {
	return strings.Join([]string{
		escapeDBIDComponent(d.Get(databaseSettingDatabaseAttr).(string)),
		escapeDBIDComponent(d.Get(databaseSettingParameterAttr).(string)),
	}, ":")
}

func escapeDBIDComponent(s string) string {
	// Order matters: escape backslashes first, then colons, so ':' inserted
	// here doesn't get re-escaped on the second pass.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `:`, `\:`)
	return s
}

// splitDBIDComponents splits the encoded ID on un-escaped ':' separators and
// undoes the backslash escaping applied by escapeDBIDComponent. It returns
// however many components are present; callers validate the count.
func splitDBIDComponents(id string) []string {
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(id); i++ {
		if id[i] == '\\' && i+1 < len(id) {
			cur.WriteByte(id[i+1])
			i++
			continue
		}
		if id[i] == ':' {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(id[i])
	}
	parts = append(parts, cur.String())
	return parts
}

func resourcePostgreSQLDatabaseSettingImport(ctx context.Context, d *schema.ResourceData, meta any) ([]*schema.ResourceData, error) {
	parts := splitDBIDComponents(d.Id())
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf(
			"invalid postgresql_database_setting import ID %q: expected <database>:<parameter>, with literal ':' and '\\' in any component backslash-escaped (use single quotes in the shell to preserve backslashes)",
			d.Id(),
		)
	}
	d.Set(databaseSettingDatabaseAttr, parts[0])
	d.Set(databaseSettingParameterAttr, parts[1])
	return []*schema.ResourceData{d}, nil
}
