package crdschemahistory

import (
	"strconv"
	"strings"
)

// Ptah v0.14.0 reports changes to the objects its feature owners plan as
// feature_changes or feature_access_<effect>, optionally followed by
// ":<kind>", the type of object, and stops writing 31 per-object categories.
// A kind is a path with dots and slashes, so the status category can no
// longer be an enum of lowercase names. Its enum gives way to a pattern that
// admits every value the enum did, the undecided category Ptah has written
// since v0.13.0, and the feature categories; its length bound rises from 64
// to 153 bytes.
//
// No stored value stops validating: every value the old schema allowed is in
// the new pattern, which TestDriftFeatureCategoriesBreakWidensTheCategory
// holds. The check cannot prove that for an arbitrary enum and pattern, so the
// two transitions are declared here.
var driftFeatureCategoriesBreak = declaredBreak{
	version: 2,
	reason: "Ptah v0.14.0's feature categories carry a kind; the status category's enum becomes a pattern " +
		"that admits every value the enum did",
	transitions: []string{
		driftCategoryPath + "the enum lost " + strings.Join(driftCategoriesBeforeFeatures, ", "),
		driftCategoryPath + "the pattern changed from " + strconv.Quote(`^[a-z][a-z0-9_]{0,63}$`) + " to " +
			strconv.Quote(driftCategoryPatternWithFeatures),
	},
}

const driftCategoryPath = "ptahschemas.operator.ptah.run: status.target.driftFindings.[].category: "

// driftCategoriesBeforeFeatures is the status enum at schema version 1.
var driftCategoriesBeforeFeatures = []string{
	"async_replications_added", "async_replications_modified", "async_replications_removed", "columns_added",
	"columns_modified", "columns_removed", "constraints_added", "constraints_removed",
	"coordination_nodes_added", "coordination_nodes_modified", "coordination_nodes_removed",
	"data_rows_deleted", "data_rows_inserted", "data_rows_updated", "enum_values_added", "enum_values_removed",
	"enums_added", "enums_removed", "extensions_added", "extensions_modified", "extensions_removed",
	"external_data_sources_added", "external_data_sources_changed", "external_data_sources_removed",
	"external_tables_added", "external_tables_changed", "external_tables_removed", "functions_added",
	"functions_modified", "functions_removed", "indexes_added", "indexes_removed",
	"resource_pool_classifiers_added", "resource_pool_classifiers_modified",
	"resource_pool_classifiers_removed", "resource_pools_added", "resource_pools_modified",
	"resource_pools_removed", "rls_enabled_tables_added", "rls_enabled_tables_removed", "rls_force_added",
	"rls_force_removed", "rls_policies_added", "rls_policies_modified", "rls_policies_removed", "roles_added",
	"roles_modified", "roles_removed", "schemas_added", "schemas_modified", "schemas_removed", "secrets_added",
	"secrets_removed", "secrets_rotated", "table_constraints_added", "table_constraints_removed",
	"tables_added", "tables_removed", "topic_consumers_removed", "topics_added", "topics_modified",
	"topics_removed", "transfers_added", "transfers_modified", "transfers_removed",
	"unique_protections_removed", "vector_dimension_changed",
}

// driftCategoryPatternWithFeatures is the status pattern at schema version 2.
const driftCategoryPatternWithFeatures = `^(async_replications_added|async_replications_modified|async_replications_removed|columns_added|columns_modified|columns_removed|constraints_added|constraints_removed|coordination_nodes_added|coordination_nodes_modified|coordination_nodes_removed|data_rows_deleted|data_rows_inserted|data_rows_updated|enum_values_added|enum_values_removed|enums_added|enums_removed|extensions_added|extensions_modified|extensions_removed|external_data_sources_added|external_data_sources_changed|external_data_sources_removed|external_tables_added|external_tables_changed|external_tables_removed|functions_added|functions_modified|functions_removed|indexes_added|indexes_removed|resource_pool_classifiers_added|resource_pool_classifiers_modified|resource_pool_classifiers_removed|resource_pools_added|resource_pools_modified|resource_pools_removed|rls_enabled_tables_added|rls_enabled_tables_removed|rls_force_added|rls_force_removed|rls_policies_added|rls_policies_modified|rls_policies_removed|roles_added|roles_modified|roles_removed|schemas_added|schemas_modified|schemas_removed|secrets_added|secrets_removed|secrets_rotated|table_constraints_added|table_constraints_removed|tables_added|tables_removed|topic_consumers_removed|topics_added|topics_modified|topics_removed|transfers_added|transfers_modified|transfers_removed|undecided|unique_protections_removed|vector_dimension_changed|feature_(changes|access_(widened|narrowed|unchanged|unknown))(:[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(/[A-Za-z0-9_-][A-Za-z0-9._-]*)+)?)$`
