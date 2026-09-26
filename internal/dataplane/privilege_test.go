package dataplane_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

func TestPrivilegeChangeVocabulary(t *testing.T) {
	t.Parallel()

	want := []string{
		"Grant", "Revoke", "RoleMembership", "Role", "Ownership",
		"RowSecurityPolicy", "SecurityDefiner", "Definer", "FunctionReplacement",
	}
	got := dataplane.PrivilegeChangeKinds()
	if !slices.Equal(got, want) {
		t.Fatalf("PrivilegeChangeKinds() = %q, want %q", got, want)
	}
	got[0] = "Mutated"
	if fresh := dataplane.PrivilegeChangeKinds(); !slices.Equal(fresh, want) {
		t.Fatalf("mutating the returned slice changed the vocabulary: %q", fresh)
	}
	for _, kind := range want {
		if !dataplane.IsKnownPrivilegeChange(kind) {
			t.Errorf("IsKnownPrivilegeChange(%q) = false", kind)
		}
	}
	for _, kind := range []string{"", "grant", "Mutated", "Superuser"} {
		if dataplane.IsKnownPrivilegeChange(kind) {
			t.Errorf("IsKnownPrivilegeChange(%q) = true", kind)
		}
	}
}

// The statements below are what Ptah's planner writes at the pinned commit, as
// the plan document stores them -- leading comment lines kept, the trailing
// semicolon dropped -- plus the shapes it cannot write today, which an artifact
// may still reach it with. Ptah rates every one of them safe except the drops,
// DISABLE and NO FORCE, which is why the operator reads them itself.
func TestDecodePlanRaisesPrivilegeChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dialect   string
		statement string
		want      []string
	}{
		// PostgreSQL privileges on objects.
		{name: "table grant", dialect: "postgres", statement: `GRANT SELECT ON TABLE "public"."orders" TO "app"`, want: []string{"Grant"}},
		{name: "column grant", dialect: "postgres", statement: `GRANT UPDATE ("state") ON TABLE "public"."orders" TO "app"`, want: []string{"Grant"}},
		{name: "routine grant", dialect: "postgres", statement: `GRANT EXECUTE ON FUNCTION "public"."get_tenant"(uuid) TO "app"`, want: []string{"Grant"}},
		{name: "grant with grant option", dialect: "postgres", statement: `GRANT USAGE ON SCHEMA "app" TO "app" WITH GRANT OPTION`, want: []string{"Grant"}},
		{name: "grant to PUBLIC", dialect: "postgres", statement: `GRANT SELECT ON TABLE "public"."orders" TO PUBLIC`, want: []string{"Grant"}},
		{name: "revoke", dialect: "postgres", statement: `REVOKE DELETE ON TABLE "public"."orders" FROM "app"`, want: []string{"Revoke"}},
		{name: "revoke grant option", dialect: "postgres", statement: `REVOKE GRANT OPTION FOR SELECT ON TABLE "public"."orders" FROM "app"`, want: []string{"Revoke"}},
		{
			name: "default privileges grant", dialect: "postgres",
			statement: `ALTER DEFAULT PRIVILEGES FOR ROLE "owner" IN SCHEMA "app" GRANT SELECT ON TABLES TO "reader"`,
			want:      []string{"Grant"},
		},
		{
			name: "default privileges revoke", dialect: "postgres",
			statement: `ALTER DEFAULT PRIVILEGES FOR ROLE "owner" IN SCHEMA "app" REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC`,
			want:      []string{"Revoke"},
		},
		// PostgreSQL roles and membership.
		{name: "role granted to a role", dialect: "postgres", statement: `GRANT "app_admin" TO "app"`, want: []string{"RoleMembership"}},
		{name: "role granted with admin option", dialect: "postgres", statement: `GRANT "app_admin" TO "app" WITH ADMIN OPTION`, want: []string{"RoleMembership"}},
		{name: "role revoked from a role", dialect: "postgres", statement: `REVOKE "app_admin" FROM "app"`, want: []string{"RoleMembership"}},
		{name: "group member added", dialect: "postgres", statement: `ALTER GROUP "admins" ADD USER "app"`, want: []string{"RoleMembership"}},
		{name: "group member removed", dialect: "postgres", statement: `ALTER GROUP "admins" DROP USER "app"`, want: []string{"RoleMembership"}},
		{
			name: "role created", dialect: "postgres",
			statement: `CREATE ROLE "app_reader" WITH NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE INHERIT NOREPLICATION`,
			want:      []string{"Role"},
		},
		{name: "role raised to superuser", dialect: "postgres", statement: "-- Modify role app attributes\nALTER ROLE \"app\" SUPERUSER", want: []string{"Role"}},
		{name: "role setting", dialect: "postgres", statement: `ALTER ROLE "app" SET search_path = pg_catalog, public`, want: []string{"Role"}},
		{name: "role dropped", dialect: "postgres", statement: `DROP ROLE IF EXISTS "app_reader"`, want: []string{"Role"}},
		// PostgreSQL ownership.
		{name: "table owner", dialect: "postgres", statement: `ALTER TABLE "public"."orders" OWNER TO "app"`, want: []string{"Ownership"}},
		{name: "function owner", dialect: "postgres", statement: `ALTER FUNCTION "public"."get_tenant"(uuid) OWNER TO "app"`, want: []string{"Ownership"}},
		{name: "a table called rename", dialect: "postgres", statement: `ALTER TABLE rename OWNER TO "app"`, want: []string{"Ownership"}},
		{name: "reassign owned", dialect: "postgres", statement: `REASSIGN OWNED BY "old_owner" TO "new_owner"`, want: []string{"Ownership"}},
		{name: "drop owned", dialect: "postgres", statement: `DROP OWNED BY "old_owner"`, want: []string{"Revoke"}},
		{name: "schema authorization", dialect: "postgres", statement: `CREATE SCHEMA "tenant" AUTHORIZATION "app"`, want: []string{"Ownership"}},
		// PostgreSQL row security. Every policy is raised: whether USING (true)
		// or USING (tenant_id = tenant_id) opens a table is not a keyword.
		{
			name: "permissive policy", dialect: "postgres",
			statement: "CREATE POLICY \"allow_all\" ON \"public\".\"orders\" FOR ALL TO PUBLIC\n    USING (true)\n    WITH CHECK (true)",
			want:      []string{"RowSecurityPolicy"},
		},
		{
			name: "restrictive policy", dialect: "postgres",
			statement: "CREATE POLICY \"tenant_only\" ON \"public\".\"orders\" AS RESTRICTIVE FOR SELECT TO \"app\", \"reporting\"\n" +
				"    USING (tenant_id = current_setting('app.tenant')::uuid)",
			want: []string{"RowSecurityPolicy"},
		},
		{name: "policy altered", dialect: "postgres", statement: `ALTER POLICY "tenant_only" ON "public"."orders" USING (true)`, want: []string{"RowSecurityPolicy"}},
		{name: "policy dropped", dialect: "postgres", statement: `DROP POLICY IF EXISTS "allow_all" ON "public"."orders"`, want: []string{"RowSecurityPolicy"}},
		{name: "row security unforced", dialect: "postgres", statement: `ALTER TABLE "public"."orders" NO FORCE ROW LEVEL SECURITY`, want: []string{"RowSecurityPolicy"}},
		{name: "row security disabled", dialect: "postgres", statement: `ALTER TABLE "public"."orders" DISABLE ROW LEVEL SECURITY`, want: []string{"RowSecurityPolicy"}},
		// PostgreSQL code that runs with someone else's rights.
		{
			name: "security definer function", dialect: "postgres",
			statement: "CREATE OR REPLACE FUNCTION \"public\".\"get_tenant\"(p uuid) RETURNS text AS $$\n" +
				"BEGIN RETURN current_setting('app.tenant', true); END;\n$$\n" +
				"LANGUAGE plpgsql SECURITY DEFINER STABLE SET search_path = pg_catalog, public",
			want: []string{"SecurityDefiner", "FunctionReplacement"},
		},
		{name: "function made security definer", dialect: "postgres", statement: `ALTER FUNCTION "public"."get_tenant"(uuid) SECURITY DEFINER`, want: []string{"SecurityDefiner"}},
		{name: "view back to owner rights", dialect: "postgres", statement: `ALTER VIEW "public"."v_orders" SET (security_invoker = false)`, want: []string{"SecurityDefiner"}},
		{name: "view option reset", dialect: "postgres", statement: `ALTER VIEW "public"."v_orders" RESET (security_invoker)`, want: []string{"SecurityDefiner"}},
		{
			// Ptah writes every function as CREATE OR REPLACE, new or not, and
			// a trigger function is no exception.
			name: "trigger function", dialect: "postgres",
			statement: "CREATE OR REPLACE FUNCTION \"ptah_trigger_public_orders_set_updated\"()\nRETURNS trigger AS $$\n" +
				"BEGIN\nNEW.updated_at = now(); RETURN NEW;\nEND;\n$$ LANGUAGE plpgsql",
			want: []string{"FunctionReplacement"},
		},
		{
			name: "procedure replaced", dialect: "postgres",
			statement: `CREATE OR REPLACE PROCEDURE "public"."archive"() LANGUAGE sql AS $$ DELETE FROM archive $$`,
			want:      []string{"FunctionReplacement"},
		},
		{
			// A body is read as SQL. The GRANT runs when the function is
			// called rather than when it is created, and it is raised anyway.
			name: "grant inside a body", dialect: "postgres",
			statement: `CREATE FUNCTION "public"."open_up"() RETURNS void LANGUAGE plpgsql AS $$ BEGIN GRANT SELECT ON orders TO PUBLIC; END $$`,
			want:      []string{"Grant"},
		},
		{
			name: "kinds from every statement in the text, in vocabulary order", dialect: "postgres",
			statement: `ALTER TABLE "public"."orders" OWNER TO "app"; GRANT SELECT ON TABLE "public"."orders" TO "app"`,
			want:      []string{"Grant", "Ownership"},
		},
		// MySQL and MariaDB.
		{name: "MySQL schema grant", dialect: "mysql", statement: "GRANT ALL ON `shop`.* TO `app` WITH GRANT OPTION", want: []string{"Grant"}},
		{name: "MySQL account grant", dialect: "mysql", statement: "GRANT SELECT ON `shop`.`orders` TO 'app'@'%'", want: []string{"Grant"}},
		{name: "MySQL grant of the grant option", dialect: "mysql", statement: "GRANT GRANT OPTION ON `shop`.* TO `app`", want: []string{"Grant"}},
		{name: "MySQL revoke", dialect: "mysql", statement: "REVOKE SELECT ON `shop`.`orders` FROM `app`", want: []string{"Revoke"}},
		{name: "MySQL revoke everything", dialect: "mysql", statement: "REVOKE ALL PRIVILEGES, GRANT OPTION FROM 'app'@'%'", want: []string{"Revoke"}},
		{name: "MySQL role granted", dialect: "mysql", statement: "GRANT `app_reader` TO `app`", want: []string{"RoleMembership"}},
		{name: "MySQL default role", dialect: "mysql", statement: "SET DEFAULT ROLE ALL TO 'app'@'%'", want: []string{"RoleMembership"}},
		{name: "MySQL role created", dialect: "mysql", statement: "CREATE ROLE IF NOT EXISTS `app_reader`", want: []string{"Role"}},
		{
			name: "MySQL security definer routine", dialect: "mysql",
			statement: "CREATE FUNCTION `get_tenant`(p int) RETURNS int READS SQL DATA SQL SECURITY DEFINER RETURN p + 1",
			want:      []string{"SecurityDefiner"},
		},
		{name: "MySQL routine made definer", dialect: "mysql", statement: "ALTER FUNCTION `get_tenant` SQL SECURITY DEFINER", want: []string{"SecurityDefiner"}},
		{
			name: "MySQL definer view", dialect: "mysql",
			statement: "CREATE DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `v_orders` AS SELECT id FROM orders",
			want:      []string{"SecurityDefiner", "Definer"},
		},
		{
			name: "MySQL definer trigger", dialect: "mysql",
			statement: "CREATE DEFINER=CURRENT_USER TRIGGER `t_new` BEFORE INSERT ON `orders` FOR EACH ROW SET NEW.created_at = NOW()",
			want:      []string{"Definer"},
		},
		{
			name: "MySQL definer in an executable comment", dialect: "mysql",
			statement: "CREATE /*!50017 DEFINER=`root`@`localhost`*/ TRIGGER `t_new` BEFORE INSERT ON `orders` FOR EACH ROW SET NEW.created_at = NOW()",
			want:      []string{"Definer"},
		},
		{name: "MySQL view definer altered", dialect: "mysql", statement: "ALTER DEFINER = `admin`@`%` VIEW `v` AS SELECT 1", want: []string{"Definer"}},
		{
			name: "MariaDB definer function replaced", dialect: "mariadb",
			statement: "CREATE OR REPLACE DEFINER=`admin`@`%` FUNCTION `f`() RETURNS int RETURN 1",
			want:      []string{"SecurityDefiner", "Definer", "FunctionReplacement"},
		},
		// A MySQL or MariaDB routine runs with its definer's rights unless it
		// says otherwise, so one that names no SQL SECURITY is a definer-rights
		// routine however plainly it is created.
		{
			name: "MySQL function with no security mode", dialect: "mysql",
			statement: "CREATE FUNCTION `get_tenant`(p int) RETURNS int READS SQL DATA RETURN p + 1",
			want:      []string{"SecurityDefiner"},
		},
		{
			name: "MySQL procedure with no security mode", dialect: "mysql",
			statement: "CREATE PROCEDURE `archive`() MODIFIES SQL DATA BEGIN DELETE FROM `archive`; END",
			want:      []string{"SecurityDefiner"},
		},
		{
			name: "MySQL function in an executable comment", dialect: "mysql",
			statement: "/*!50003 CREATE FUNCTION `f`() RETURNS int DETERMINISTIC RETURN 1 */",
			want:      []string{"SecurityDefiner"},
		},
		{
			name: "MariaDB invoker function replaced", dialect: "mariadb",
			statement: "CREATE OR REPLACE FUNCTION `f`() RETURNS int SQL SECURITY INVOKER RETURN 1",
			want:      []string{"FunctionReplacement"},
		},
		// The session's identity, which everything after it in the Apply runs as.
		{name: "role assumed", dialect: "postgres", statement: `SET ROLE "app_admin"`, want: []string{"Role"}},
		{name: "role assumed for the transaction", dialect: "postgres", statement: `SET LOCAL ROLE "app_admin"`, want: []string{"Role"}},
		{name: "session authorization", dialect: "postgres", statement: `SET SESSION AUTHORIZATION "app_admin"`, want: []string{"Role"}},
		{
			name: "session authorization for the session", dialect: "postgres",
			statement: `SET SESSION SESSION AUTHORIZATION "app_admin"`, want: []string{"Role"},
		},
		{name: "role assumed in a DO block", dialect: "postgres", statement: `DO $$ BEGIN SET ROLE app_admin; END $$`, want: []string{"Role"}},
		{name: "MySQL role assumed", dialect: "mysql", statement: "SET ROLE ALL", want: []string{"Role"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			decoded, err := dataplane.DecodePlan(planDocument(test.dialect, test.statement), engineOf(test.dialect))
			if err != nil {
				t.Fatalf("DecodePlan(%q) error = %v", test.statement, err)
			}
			if !slices.Equal(decoded.PrivilegeChanges, test.want) {
				t.Fatalf("DecodePlan(%q).PrivilegeChanges = %q, want %q", test.statement, decoded.PrivilegeChanges, test.want)
			}
		})
	}
}

// Near-misses: statements that carry the words without changing any authority.
// Each is a shape a real plan holds -- a column called role or owner, a
// renamed column, a comment, a declared row -- and raising it would make
// apply: Always wait for a person over nothing.
func TestDecodePlanDoesNotRaiseLookalikes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dialect   string
		statement string
	}{
		{name: "columns named for privileges", dialect: "postgres", statement: `CREATE TABLE grants (grant_id bigint, role text, owner text, policy text, definer text)`},
		{name: "quoted names", dialect: "postgres", statement: `CREATE TABLE "public"."grant" ("owner" text, "security" text)`},
		{name: "role column default", dialect: "postgres", statement: `ALTER TABLE accounts ALTER role SET DEFAULT 'member'`},
		{name: "policy column dropped", dialect: "postgres", statement: `ALTER TABLE accounts DROP policy`},
		{name: "owner column added", dialect: "postgres", statement: `ALTER TABLE accounts ADD COLUMN owner text`},
		{name: "owner column renamed", dialect: "postgres", statement: `ALTER TABLE accounts RENAME COLUMN owner TO proprietor`},
		{name: "owner column renamed without COLUMN", dialect: "postgres", statement: `ALTER TABLE accounts RENAME owner TO proprietor`},
		{name: "owner constraint renamed", dialect: "postgres", statement: `ALTER TABLE accounts RENAME CONSTRAINT owner TO owner_present`},
		{name: "owner attribute renamed", dialect: "postgres", statement: `ALTER TYPE address RENAME ATTRIBUTE owner TO holder`},
		{name: "sequence owned by a column", dialect: "postgres", statement: `ALTER SEQUENCE "public"."orders_id_seq" OWNED BY "public"."orders"."id"`},
		{name: "role comment", dialect: "postgres", statement: `COMMENT ON ROLE "app" IS 'grant it wisely'`},
		{name: "policy comment", dialect: "postgres", statement: `COMMENT ON POLICY "tenant_only" ON "public"."orders" IS 'owner to review'`},
		{name: "row security enabled", dialect: "postgres", statement: `ALTER TABLE "public"."orders" ENABLE ROW LEVEL SECURITY`},
		{name: "row security forced", dialect: "postgres", statement: `ALTER TABLE "public"."orders" FORCE ROW LEVEL SECURITY`},
		{
			name: "invoker function created", dialect: "postgres",
			statement: `CREATE FUNCTION "public"."total"(a int) RETURNS int LANGUAGE sql SECURITY INVOKER AS $$ SELECT a $$`,
		},
		{name: "view invoker rights", dialect: "postgres", statement: `ALTER VIEW "public"."v_orders" SET (security_invoker = true)`},
		{name: "view replaced", dialect: "postgres", statement: "CREATE OR REPLACE VIEW \"public\".\"v_orders\" AS\nSELECT id FROM public.orders"},
		{
			name: "trigger replaced", dialect: "postgres",
			statement: `CREATE OR REPLACE TRIGGER "set_updated" BEFORE UPDATE ON "public"."orders" FOR EACH ROW EXECUTE FUNCTION "ptah_trigger_public_orders_set_updated"()`,
		},
		{name: "declared row", dialect: "postgres", statement: `INSERT INTO "permissions" ("action", "scope") VALUES ('GRANT', 'SECURITY DEFINER')`},
		{name: "keyword prefixes of names", dialect: "postgres", statement: `CREATE INDEX grant2_idx ON ledger (grant2, owner$id)`},
		{name: "commented out", dialect: "postgres", statement: "-- GRANT SELECT ON orders TO PUBLIC\nCREATE TABLE t (id int)"},
		{name: "keywords in a default", dialect: "postgres", statement: `CREATE TABLE t (note text DEFAULT 'ALTER TABLE t OWNER TO app')`},
		{
			name: "MySQL invoker routine", dialect: "mysql",
			statement: "CREATE FUNCTION `total`(p int) RETURNS int DETERMINISTIC SQL SECURITY INVOKER RETURN p",
		},
		{name: "MySQL view replaced", dialect: "mysql", statement: "CREATE OR REPLACE VIEW `v_mod` AS\nSELECT id, total FROM orders"},
		{name: "MySQL definer column added", dialect: "mysql", statement: "ALTER TABLE `audit` ADD COLUMN definer varchar(64)"},
		{name: "MySQL definer and event columns", dialect: "mysql", statement: "CREATE TABLE audit (definer varchar(64), event varchar(64))"},
		{name: "MySQL role column default", dialect: "mysql", statement: "ALTER TABLE accounts ALTER role SET DEFAULT 'member'"},
		{name: "MySQL skip comment", dialect: "mysql", statement: "-- CREATE POLICY p not supported in MySQL"},
		{name: "MySQL hash comment", dialect: "mysql", statement: "# GRANT ALL ON *.* TO app\nCREATE TABLE t (id int)"},
		{name: "MySQL declared row", dialect: "mysql", statement: "INSERT INTO `permissions` (`action`) VALUES ('GRANT ALL ON *.*')"},
		{
			name: "MySQL invoker procedure with a body", dialect: "mysql",
			statement: "CREATE PROCEDURE `archive`() MODIFIES SQL DATA SQL SECURITY INVOKER BEGIN DELETE FROM `archive`; END",
		},
		{name: "MySQL table named for routines", dialect: "mysql", statement: "CREATE TABLE `functions` (`id` int, `procedure` text)"},
		{name: "MySQL session setting", dialect: "mysql", statement: "SET SESSION sql_mode = 'ANSI_QUOTES'"},
		// PostgreSQL defaults a function to SECURITY INVOKER, so the MySQL
		// reading of a routine with no mode does not reach it.
		{name: "function with no security mode", dialect: "postgres", statement: `CREATE FUNCTION "public"."total"(a int) RETURNS int LANGUAGE sql AS $$ SELECT a $$`},
		{name: "a column called role updated", dialect: "postgres", statement: `UPDATE accounts SET role = 'admin' WHERE id = 1`},
		{
			name: "a column called role upserted", dialect: "postgres",
			statement: `INSERT INTO accounts (id, role) VALUES (1, 'member') ON CONFLICT (id) DO UPDATE SET role = EXCLUDED.role`,
		},
		{name: "a search path set", dialect: "postgres", statement: `SET search_path = public`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			decoded, err := dataplane.DecodePlan(planDocument(test.dialect, test.statement), engineOf(test.dialect))
			if err != nil {
				t.Fatalf("DecodePlan(%q) error = %v", test.statement, err)
			}
			if len(decoded.PrivilegeChanges) != 0 {
				t.Fatalf("DecodePlan(%q).PrivilegeChanges = %q, want none", test.statement, decoded.PrivilegeChanges)
			}
		})
	}
}

// Definer rights that come from an engine default rather than from the
// statement are outside the class, and this pins that the classifier reads the
// statement: a MySQL view or trigger that names no definer and no SQL SECURITY
// still runs as its definer, and the documentation says so rather than this
// classifier guessing.
func TestDecodePlanDoesNotRaiseImplicitDefinerRights(t *testing.T) {
	t.Parallel()

	for _, test := range []struct{ dialect, statement string }{
		{dialect: "mysql", statement: "CREATE VIEW `v_orders` AS SELECT id FROM orders"},
		{dialect: "mysql", statement: "CREATE TRIGGER `t_new` BEFORE INSERT ON `orders` FOR EACH ROW SET NEW.created_at = NOW()"},
		{dialect: "postgres", statement: "CREATE VIEW \"public\".\"v_orders\" AS\nSELECT id FROM public.orders"},
	} {
		decoded, err := dataplane.DecodePlan(planDocument(test.dialect, test.statement), engineOf(test.dialect))
		if err != nil {
			t.Fatalf("DecodePlan(%q) error = %v", test.statement, err)
		}
		if len(decoded.PrivilegeChanges) != 0 {
			t.Fatalf("DecodePlan(%q).PrivilegeChanges = %q, want none", test.statement, decoded.PrivilegeChanges)
		}
	}
}

// Each of these hides a clause from a lexer that reads the statement one way
// while the server reads it another. The server executes the hidden clause; the
// operator has to see it.
func TestDecodePlanReadsPastLexicalTricks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dialect   string
		statement string
		want      []string
	}{
		{
			name: "quote in a comment inside a dollar-quoted body", dialect: "postgres",
			statement: "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 -- ' $$ SECURITY DEFINER",
			want:      []string{"SecurityDefiner"},
		},
		{
			name: "tagged dollar quote", dialect: "postgres",
			statement: "CREATE FUNCTION f() RETURNS text LANGUAGE sql AS $body$ SELECT 'it''s' || $$'$$ $body$ SECURITY DEFINER",
			want:      []string{"SecurityDefiner"},
		},
		{
			name: "nested block comment", dialect: "postgres",
			statement: "CREATE FUNCTION f() RETURNS int LANGUAGE sql /* outer /* inner */ ' */ SECURITY DEFINER AS $$ SELECT 1 $$",
			want:      []string{"SecurityDefiner"},
		},
		{
			// Read with standard_conforming_strings on, as the server does:
			// the E string escapes its quote and the plain one does not. Read
			// with every string escaped, or with none, one of the two runs on
			// to the end of the statement.
			name: "escape string", dialect: "postgres",
			statement: `CREATE FUNCTION f(a text DEFAULT E'\'', b text DEFAULT '\') RETURNS text LANGUAGE sql SECURITY DEFINER AS $$ SELECT a $$`,
			want:      []string{"SecurityDefiner"},
		},
		{
			// Valid only while standard_conforming_strings is off, where the
			// backslash escapes the quote and SECURITY DEFINER is a clause.
			name: "standard_conforming_strings off", dialect: "postgres",
			statement: `CREATE FUNCTION f(a text DEFAULT '\'') RETURNS text LANGUAGE sql SECURITY DEFINER AS $$ SELECT a $$`,
			want:      []string{"SecurityDefiner"},
		},
		{
			name: "backtick is an operator character in PostgreSQL", dialect: "postgres",
			statement: "SELECT 1 ` 2; GRANT ALL ON orders TO PUBLIC",
			want:      []string{"Grant"},
		},
		{
			name: "quote in a MySQL hash comment", dialect: "mysql",
			statement: "SELECT 1 # don't\n; GRANT ALL ON `shop`.* TO `app`",
			want:      []string{"Grant"},
		},
		{
			name: "NO_BACKSLASH_ESCAPES", dialect: "mysql",
			statement: `SELECT 'prefix\'; GRANT ALL ON shop.* TO app`,
			want:      []string{"Grant"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			decoded, err := dataplane.DecodePlan(planDocument(test.dialect, test.statement), engineOf(test.dialect))
			if err != nil {
				t.Fatalf("DecodePlan(%q) error = %v", test.statement, err)
			}
			if !slices.Equal(decoded.PrivilegeChanges, test.want) {
				t.Fatalf("DecodePlan(%q).PrivilegeChanges = %q, want %q", test.statement, decoded.PrivilegeChanges, test.want)
			}
		})
	}
}

// The destructive reading shares the lexer, so what the lexer now reads past
// is raised there as well.
func TestDecodePlanElevatesDestructiveSQLBehindLexicalTricks(t *testing.T) {
	t.Parallel()

	for _, statement := range []string{
		"CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 -- ' $$; DROP TABLE audit_log",
		"SELECT 1 /* outer /* inner */ ' */; DROP TABLE audit_log",
		`SELECT E'\''; DROP TABLE audit_log`,
	} {
		decoded, err := dataplane.DecodePlan(planDocument("postgres", statement), "PostgreSQL")
		if err != nil {
			t.Fatal(err)
		}
		if !decoded.Destructive || decoded.Statements[0].Severity != "destructive" {
			t.Fatalf("DecodePlan(%q) = %#v, want destructive elevation", statement, decoded)
		}
	}
}

// The class is the operator's own, so nothing in Ptah's document lowers it:
// not a severity, not destructive: false, and not a field of the same name,
// which the strict decoder refuses rather than reads.
func TestPrivilegeClassIsRaiseOnly(t *testing.T) {
	t.Parallel()

	document := string(planDocument("postgres", `GRANT SELECT ON TABLE "public"."orders" TO "app"`))
	decoded, err := dataplane.DecodePlan([]byte(document), "PostgreSQL")
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Destructive || decoded.Statements[0].Severity != "safe" {
		t.Fatalf("a grant changed the destructive reading: %#v", decoded)
	}
	if !slices.Equal(decoded.PrivilegeChanges, []string{"Grant"}) {
		t.Fatalf("PrivilegeChanges = %q, want [Grant] from a statement Ptah rated safe", decoded.PrivilegeChanges)
	}

	for _, field := range []string{`"PrivilegeChanges":[]`, `"privilege_changes":[]`, `"privileged":false`} {
		supplied := strings.Replace(document, `"destructive":false`, `"destructive":false,`+field, 1)
		if _, err := dataplane.DecodePlan([]byte(supplied), "PostgreSQL"); err == nil {
			t.Fatalf("DecodePlan() accepted a document that supplies %s", field)
		}
	}
}

func engineOf(dialect string) string {
	if dialect == "postgres" {
		return "PostgreSQL"
	}
	return "MySQL"
}
