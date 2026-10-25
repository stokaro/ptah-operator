package crdschemahistory

import "strconv"

// Ptah v0.16.0 compares more kinds of object and reports 22 categories for
// them: comments, grants and grant options, index visibility and renames,
// materialized views, role memberships, sequences, triggers, views and
// validated constraints. The status category's pattern admits them beside
// every category it admitted before, so no stored value stops validating,
// which TestDriftCategoriesV016BreakWidensTheCategory holds. The check cannot
// prove that for two arbitrary patterns, so the transition is declared here.
var driftCategoriesV016Break = declaredBreak{
	version: 9,
	reason:  "Ptah v0.16.0's drift categories join the status category's pattern, which still admits every value it did",
	transitions: []string{
		driftCategoryPath + "the pattern changed from " + strconv.Quote(driftCategoryPatternWithFeatures) + " to " +
			strconv.Quote(driftCategoryPatternV016),
	},
}

// driftCategoriesAddedInV016 are the categories Ptah v0.16.0 added.
var driftCategoriesAddedInV016 = []string{
	"comments_changed", "constraints_validated", "grant_options_added", "grant_options_removed",
	"grants_added", "grants_removed", "index_visibility_changed", "indexes_renamed",
	"materialized_views_added", "materialized_views_modified", "materialized_views_removed", "role_memberships_added",
	"role_memberships_removed", "sequences_added", "sequences_modified", "sequences_removed",
	"triggers_added", "triggers_modified", "triggers_removed", "views_added",
	"views_modified", "views_removed",
}

// driftCategoryPatternV016 is the status pattern at schema version 9.
const driftCategoryPatternV016 = `^(async_replications_added|async_replications_modified|async_replications_removed|columns_added|columns_modified|columns_removed|comments_changed|constraints_added|constraints_removed|constraints_validated|coordination_nodes_added|coordination_nodes_modified|coordination_nodes_removed|data_rows_deleted|data_rows_inserted|data_rows_updated|enum_values_added|enum_values_removed|enums_added|enums_removed|extensions_added|extensions_modified|extensions_removed|external_data_sources_added|external_data_sources_changed|external_data_sources_removed|external_tables_added|external_tables_changed|external_tables_removed|functions_added|functions_modified|functions_removed|grant_options_added|grant_options_removed|grants_added|grants_removed|index_visibility_changed|indexes_added|indexes_removed|indexes_renamed|materialized_views_added|materialized_views_modified|materialized_views_removed|resource_pool_classifiers_added|resource_pool_classifiers_modified|resource_pool_classifiers_removed|resource_pools_added|resource_pools_modified|resource_pools_removed|rls_enabled_tables_added|rls_enabled_tables_removed|rls_force_added|rls_force_removed|rls_policies_added|rls_policies_modified|rls_policies_removed|role_memberships_added|role_memberships_removed|roles_added|roles_modified|roles_removed|schemas_added|schemas_modified|schemas_removed|secrets_added|secrets_removed|secrets_rotated|sequences_added|sequences_modified|sequences_removed|table_constraints_added|table_constraints_removed|tables_added|tables_removed|topic_consumers_removed|topics_added|topics_modified|topics_removed|transfers_added|transfers_modified|transfers_removed|triggers_added|triggers_modified|triggers_removed|undecided|unique_protections_removed|vector_dimension_changed|views_added|views_modified|views_removed|feature_(changes|access_(widened|narrowed|unchanged|unknown))(:[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(/[A-Za-z0-9_-][A-Za-z0-9._-]*)+)?)$`
