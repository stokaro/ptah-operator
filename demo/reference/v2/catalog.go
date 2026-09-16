// Package catalog is the storefront's reference data: the tables whose rows the
// application reads and never writes, declared beside the tables themselves.
//
// The module compiles it and nothing imports it: what reads it is Ptah, which
// parses the annotations out of the source. It lives under demo/ rather than
// testdata/ because a reader publishes it by typing the path, and the path is
// part of what the demonstration shows.
//
// Two tables joined by a foreign key, because a reference set is usually
// several tables that have to arrive in an order the constraint allows.
package catalog

//ptah:schema:table name="regions"
//ptah:schema:data table="regions" key="code" file="regions.yaml"
type Region struct {
	//ptah:schema:field name="code" type="VARCHAR(8)" primary="true"
	Code string

	//ptah:schema:field name="name" type="VARCHAR(64)" not_null="true"
	Name string
}

// The country rows arrive in the next revision rather than here. Ptah emits
// declared rows grouped by table, in an order that ignores the dependency
// order it uses for the tables themselves, so a child row can be offered
// before the parent row it references and the foreign key refuses it
// (stokaro/ptah#3252). Declaring the parent first, and the child once the
// parent rows exist, is the sequence that works today.
//
//ptah:schema:table name="countries"
//ptah:schema:data table="countries" key="code" file="countries.yaml"
type Country struct {
	//ptah:schema:field name="code" type="VARCHAR(2)" primary="true"
	Code string

	//ptah:schema:field name="name" type="VARCHAR(64)" not_null="true"
	Name string

	//ptah:schema:field name="region_code" type="VARCHAR(8)" not_null="true" foreign="regions(code)" foreign_key_name="fk_countries_region"
	RegionCode string
}
