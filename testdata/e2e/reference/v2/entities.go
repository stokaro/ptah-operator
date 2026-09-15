// Package reference is the declared schema and the declared rows the
// reference-data end-to-end phase publishes. It is fixture material read as
// source by Ptah, not code this module compiles: it lives under testdata so
// the Go toolchain leaves it alone.
//
// Two tables joined by a foreign key, because a reference set is usually
// several tables that have to arrive in an order the constraint allows.
package reference

//ptah:schema:table name="regions"
//ptah:schema:data table="regions" key="code" file="regions.yaml"
type Region struct {
	//ptah:schema:field name="code" type="VARCHAR(8)" primary="true"
	Code string

	//ptah:schema:field name="name" type="VARCHAR(64)" not_null="true"
	Name string
}

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
