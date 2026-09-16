// Package catalog is the storefront's reference data: the tables whose rows the
// application reads and never writes, declared beside the tables themselves.
//
// The module compiles it and nothing imports it: what reads it is Ptah, which
// parses the annotations out of the source. It lives under demo/ rather than
// testdata/ because a reader publishes it by typing the path, and the path is
// part of what the demonstration shows.
//
// The declaration on countries is gone; the table and its rows are not.
package catalog

//ptah:schema:table name="regions"
//ptah:schema:data table="regions" key="code" file="regions.yaml"
type Region struct {
	//ptah:schema:field name="code" type="VARCHAR(8)" primary="true"
	Code string

	//ptah:schema:field name="name" type="VARCHAR(64)" not_null="true"
	Name string
}

// This revision drops the row declaration on countries and keeps the table.
// Ending management is not deleting rows: what the operator stops doing is
// reconciling them, and what it leaves behind is exactly what was there.
//
//ptah:schema:table name="countries"
type Country struct {
	//ptah:schema:field name="code" type="VARCHAR(2)" primary="true"
	Code string

	//ptah:schema:field name="name" type="VARCHAR(64)" not_null="true"
	Name string

	//ptah:schema:field name="region_code" type="VARCHAR(8)" not_null="true" foreign="regions(code)" foreign_key_name="fk_countries_region"
	RegionCode string
}
