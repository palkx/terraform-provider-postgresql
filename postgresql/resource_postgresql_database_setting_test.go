package postgresql

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lib/pq"
)

// TestDatabaseSettingIDRoundTrip checks that the composite resource ID
// encodes and decodes losslessly for identifiers that may legally contain
// ':' (PostgreSQL allows it in quoted identifiers) and '\'.
func TestDatabaseSettingIDRoundTrip(t *testing.T) {
	cases := []struct {
		name                string
		database, parameter string
		wantEncoded         string
	}{
		{
			name:        "no special chars",
			database:    "app_db",
			parameter:   "role",
			wantEncoded: "app_db:role",
		},
		{
			name:        "colon in database",
			database:    "app:blue",
			parameter:   "role",
			wantEncoded: `app\:blue:role`,
		},
		{
			name:        "colon in role",
			database:    "ap:p",
			parameter:   "rol:e",
			wantEncoded: `ap\:p:rol\:e`,
		},
		{
			name:        "colons in role and database",
			database:    "app:blue",
			parameter:   "role",
			wantEncoded: `app\:blue:role`,
		},
		{
			name:        "backslash in role",
			database:    `ap\p_db`,
			parameter:   "role",
			wantEncoded: `ap\\p_db:role`,
		},
		{
			name:        "backslash and colon mixed",
			database:    `with:colon\and\backslash:app`,
			parameter:   "role",
			wantEncoded: `with\:colon\\and\\backslash\:app:role`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := strings.Join([]string{
				escapeDBIDComponent(tc.database),
				escapeDBIDComponent(tc.parameter),
			}, ":")
			if encoded != tc.wantEncoded {
				t.Fatalf("encoded = %q, want %q", encoded, tc.wantEncoded)
			}
			parts := splitIDComponents(encoded)
			if len(parts) != 2 {
				t.Fatalf("split returned %d parts, want 2: %v", len(parts), parts)
			}
			if parts[0] != tc.database || parts[1] != tc.parameter {
				t.Fatalf("round-trip mismatch: got %q/%q, want %q/%q",
					parts[0], parts[1], tc.database, tc.parameter)
			}
		})
	}
}

// TestFindSetconfigValue exercises the pure parser against the formats
// PostgreSQL actually produces in pg_db_role_setting.setconfig (verified
// empirically on PG 16). It runs without a live database.
func TestFindSetconfigDBValue(t *testing.T) {
	cases := []struct {
		name      string
		setconfig []string
		parameter string
		want      string
		wantFound bool
	}{
		{
			name:      "unwrapped plain identifier",
			setconfig: []string{"role=app_db_owner"},
			parameter: "role",
			want:      "app_db_owner",
			wantFound: true,
		},
		{
			name:      "wrapped value with comma and space",
			setconfig: []string{`search_path="app, public"`},
			parameter: "search_path",
			want:      "app, public",
			wantFound: true,
		},
		{
			name:      "wrapped value with embedded double quote (doubled inside)",
			setconfig: []string{`search_path="""a,b"", public"`},
			parameter: "search_path",
			want:      `"a,b", public`,
			wantFound: true,
		},
		{
			name: "multiple parameters, pick the requested one",
			setconfig: []string{
				`search_path="app, public"`,
				"role=app_db_owner",
			},
			parameter: "role",
			want:      "app_db_owner",
			wantFound: true,
		},
		{
			name:      "case-insensitive parameter match",
			setconfig: []string{"role=app_db_owner"},
			parameter: "ROLE",
			want:      "app_db_owner",
			wantFound: true,
		},
		{
			name:      "parameter not present",
			setconfig: []string{"role=app_db_owner"},
			parameter: "search_path",
			wantFound: false,
		},
		{
			name:      "unwrapped value containing literal quote and comma (postgres leaves untouched)",
			setconfig: []string{`application_name=has"quote,comma`},
			parameter: "application_name",
			want:      `has"quote,comma`,
			wantFound: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := findSetconfigDBValue(tc.setconfig, tc.parameter)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if got != tc.want {
				t.Fatalf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAccPostgresqlDatabaseSetting_Basic(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseSettingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlDatabaseSettingBasicConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseSettingValue(
						"rds_test_db", "role", "rds_test_owner",
					),
					resource.TestCheckResourceAttr(
						"postgresql_database_setting.assume", "value", "rds_test_owner",
					),
					resource.TestCheckResourceAttr(
						"postgresql_database_setting.assume", "id",
						"rds_test_db:role",
					),
				),
			},
		},
	})
}

func TestAccPostgresqlDatabaseSetting_UpdateValue(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseSettingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlDatabaseSettingSearchPathConfig("public"),
				Check: testAccCheckPostgresqlDatabaseSettingValue(
					"rds_test_db", "search_path", "public",
				),
			},
			{
				Config: testAccPostgresqlDatabaseSettingSearchPathConfig("app, public"),
				Check: testAccCheckPostgresqlDatabaseSettingValue(
					"rds_test_db", "search_path", "app, public",
				),
			},
		},
	})
}

func TestAccPostgresqlDatabaseSetting_MultipleParameters(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseSettingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlDatabaseSettingMultiConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseSettingValue(
						"rds_test_db", "role", "rds_test_owner",
					),
					testAccCheckPostgresqlDatabaseSettingValue(
						"rds_test_db", "search_path", "shared, app, public",
					),
				),
			},
		},
	})
}

// TestAccPostgresqlDatabaseSetting_EmbeddedQuoteValue exercises the
// catalog round-trip for a search_path value containing an embedded double
// quote. PostgreSQL stores this in pg_db_role_setting using the wrapped form
// with doubled inner quotes (`search_path="""a,b"", public"`). The Read path
// must decode `""` → `"`, otherwise terraform plan reports a permanent
// false-positive drift on every refresh.
func TestAccPostgresqlDatabaseSetting_EmbeddedQuoteValue(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseSettingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlDatabaseSettingSearchPathConfig(`"a,b", public`),
				Check: testAccCheckPostgresqlDatabaseSettingValue(
					"rds_test_db", "search_path", `"a,b", public`,
				),
			},
			// A second step with the same config asserts no drift: if Read
			// returned a mangled value, terraform would propose a re-apply
			// here and the test would fail.
			{
				Config:   testAccPostgresqlDatabaseSettingSearchPathConfig(`"a,b", public`),
				PlanOnly: true,
			},
		},
	})
}

// TestAccPostgresqlDatabaseSetting_ColonInIdentifier verifies the
// resource handles database names containing ':' end-to-end: apply,
// state ID encoding, and import round-trip via ImportStateVerify.
func TestAccPostgresqlDatabaseSetting_ColonInIdentifier(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseSettingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlDatabaseSettingColonConfig,
				Check: resource.ComposeTestCheckFunc(
					testAccCheckPostgresqlDatabaseSettingValue(
						"rds_app:blue_db", "role", "rds_test_owner",
					),
					resource.TestCheckResourceAttr(
						"postgresql_database_setting.assume", "id",
						`rds_app\:blue_db:role`,
					),
				),
			},
			{
				ResourceName:      "postgresql_database_setting.assume",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccPostgresqlDatabaseSetting_Import(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck: func() {
			testAccPreCheck(t)
			testSuperuserPreCheck(t)
		},
		Providers:    testAccProviders,
		CheckDestroy: testAccCheckPostgresqlDatabaseSettingDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccPostgresqlDatabaseSettingBasicConfig,
			},
			{
				ResourceName:      "postgresql_database_setting.assume",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccCheckPostgresqlDatabaseSettingValue(database, parameter, expected string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client := testAccProvider.Meta().(*Client)
		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		var setconfig []string
		err = txn.QueryRow(`
SELECT s.setconfig
FROM pg_db_role_setting s
JOIN pg_database d ON d.oid = s.setdatabase
WHERE d.datname = $1`, database).Scan(pq.Array(&setconfig))
		if err == sql.ErrNoRows {
			return fmt.Errorf("no pg_db_role_setting row for database %q", database)
		}
		if err != nil {
			return fmt.Errorf("error reading pg_db_role_setting: %w", err)
		}

		got, found := findSetconfigValue(setconfig, parameter)
		if !found {
			return fmt.Errorf("parameter %q not found in setconfig %v for (%s)", parameter, setconfig, database)
		}
		if got != expected {
			return fmt.Errorf("parameter %q for (%s) = %q, want %q", parameter, database, got, expected)
		}
		return nil
	}
}

func testAccCheckPostgresqlDatabaseSettingDestroy(s *terraform.State) error {
	client := testAccProvider.Meta().(*Client)

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "postgresql_database_setting" {
			continue
		}

		txn, err := startTransaction(client, "")
		if err != nil {
			return err
		}
		defer deferredRollback(txn)

		parts := splitIDComponents(rs.Primary.ID)
		if len(parts) != 3 {
			return fmt.Errorf("malformed resource ID %q", rs.Primary.ID)
		}
		database, parameter := parts[0], parts[1]

		var setconfig []string
		err = txn.QueryRow(`
SELECT s.setconfig
FROM pg_db_role_setting s
JOIN pg_database d ON d.oid = s.setdatabase
WHERE d.datname = $1`, database).Scan(pq.Array(&setconfig))
		switch {
		case err == sql.ErrNoRows:
			// Either the row was removed entirely (RESET of last param) or
			// the database itself was dropped — either way the resource
			// is gone.
			continue
		case err != nil:
			return fmt.Errorf("error reading pg_db_role_setting: %w", err)
		}

		if _, found := findSetconfigValue(setconfig, parameter); found {
			return fmt.Errorf(
				"database setting %s for (%s) still exists after destroy",
				parameter, database,
			)
		}
	}
	return nil
}

const testAccPostgresqlDatabaseSettingBasicConfig = `
resource "postgresql_role" "owner" {
  name  = "rds_test_owner"
  login = false
}

resource "postgresql_role" "user" {
  name    = "rds_test_owner@example.com"
  login   = true
  inherit = true
  roles   = [postgresql_role.owner.name]
}

resource "postgresql_database" "db" {
  name              = "rds_test_db"
  owner             = postgresql_role.owner.name
  allow_connections = true
}

resource "postgresql_database_setting" "assume" {
  database  = postgresql_database.db.name
  parameter = "role"
  value     = postgresql_role.owner.name
}
`

func testAccPostgresqlDatabaseSettingSearchPathConfig(searchPath string) string {
	return fmt.Sprintf(`
resource "postgresql_role" "owner" {
  name  = "rds_test_owner"
  login = false
}

resource "postgresql_role" "user" {
  name    = "rds_test_owner@example.com"
  login   = true
  inherit = true
  roles   = [postgresql_role.owner.name]
}

resource "postgresql_database" "db" {
  name              = "rds_test_db"
  owner             = postgresql_role.owner.name
  allow_connections = true
}

resource "postgresql_database_setting" "search_path" {
  database  = postgresql_database.db.name
  parameter = "search_path"
  value     = %q
}
`, searchPath)
}

const testAccPostgresqlDatabaseSettingColonConfig = `
resource "postgresql_role" "owner" {
  name  = "rds_test_owner"
  login = false
}

resource "postgresql_role" "user" {
  name    = "alice:dev"
  login   = true
  inherit = true
  roles   = [postgresql_role.owner.name]
}

resource "postgresql_database" "db" {
  name              = "rds_app:blue_db"
  owner             = postgresql_role.owner.name
  allow_connections = true
}

resource "postgresql_database_setting" "assume" {
  database  = postgresql_database.db.name
  parameter = "role"
  value     = postgresql_role.owner.name
}
`

const testAccPostgresqlDatabaseSettingMultiConfig = `
resource "postgresql_role" "owner" {
  name  = "rds_test_owner"
  login = false
}

resource "postgresql_role" "user" {
  name    = "rds_test_owner@example.com"
  login   = true
  inherit = true
  roles   = [postgresql_role.owner.name]
}

resource "postgresql_database" "db" {
  name              = "rds_test_db"
  owner             = postgresql_role.owner.name
  allow_connections = true
}

resource "postgresql_database_setting" "assume" {
  database  = postgresql_database.db.name
  parameter = "role"
  value     = postgresql_role.owner.name
}

resource "postgresql_database_setting" "search_path" {
  database  = postgresql_database.db.name
  parameter = "search_path"
  value     = "shared, app, public"
}
`
