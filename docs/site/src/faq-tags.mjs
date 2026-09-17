// Which tags each FAQ question carries, keyed by the anchor the question
// declares. Kept beside sidebar.mjs and for the same reason: a plain Node
// script and Astro both read it, so there is no second copy to drift.
//
// The vocabulary is the one docs.ptah.run uses, wherever a facet means the same
// thing on both sites: a reader who moves between them meets one set of names.
// `install`, `status` and `database-access` are the operator's own, because the
// chart, the condition tuple and the route to the database are questions the
// CLI does not have.
//
// Tagging is editorial. A tag belongs on a question when a reader would filter
// by it to find that answer, not when the words happen to appear in it.
//
// scripts/check-faq-groups.mjs holds this to the page: every anchor here must
// exist on the built page, every question must carry at least one tag, and
// every tag must be labelled and used.

/** @type {Record<string, string[]>} */
export const questionTags = {
  "what-the-operator-does": ["getting-started", "scope"],
  "ptahschema-and-migration-directory": ["migrations", "scope"],
  "operator-does-not-provision": ["scope", "database-access"],
  "is-it-production-ready": ["getting-started", "compatibility"],
  "operator-version-selector": ["compatibility", "troubleshooting"],
  "three-values-with-no-default": ["install", "configuration"],
  "upgrade-cli-and-operator-together": ["install", "compatibility"],
  "untested-is-not-incompatible": ["compatibility"],
  "kubernetes-version-window": ["kubernetes", "compatibility"],
  "registry-secret-authority": ["oci", "security", "troubleshooting"],
  "moved-tag-invalidates-approval": ["oci", "planning"],
  "digest-pin-refuses-a-tag": ["oci", "security"],
  "registry-outage-fail-closed": ["oci", "status", "safety"],
  "approval-rejected": ["planning", "security"],
  "reviewer-cannot-read-sql": ["planning", "security"],
  "read-a-plan": ["planning"],
  "plan-recorded-not-applied": ["planning", "safety", "status"],
  "plan-too-large": ["planning", "troubleshooting"],
  "old-plan-objects": ["planning", "history"],
  "apply-outcome-unknown": ["recovery", "safety", "status"],
  "no-automatic-rollback": ["recovery", "safety"],
  "suspend-blocks-the-realm": ["scope", "safety"],
  "deleting-never-drops-data": ["data", "safety"],
  "unsupported-engine": ["compatibility", "status", "troubleshooting"],
  "one-database-two-urls": ["database-access", "configuration"],
  "least-privilege-login": ["database-access", "security"],
  "controller-and-credentials": ["database-access", "security"],
  "reading-convergence-status": ["status"],
  "reporting-an-operator-bug": ["community", "troubleshooting"],
};

// The reader-facing name for each tag. A tag with no entry fails the gate:
// an unexplained facet is a filter nobody can use on purpose.
/** @type {Record<string, string>} */
export const tagLabels = {
  "compatibility": "Compatibility",
  "configuration": "Configuration",
  "community": "Contributing",
  "data": "Data",
  "database-access": "Database access",
  "getting-started": "Getting started",
  "history": "History",
  "install": "Install and upgrade",
  "kubernetes": "Kubernetes",
  "migrations": "Migrations",
  "oci": "OCI",
  "planning": "Plan and approve",
  "recovery": "Recovery",
  "safety": "Safety",
  "scope": "Scope",
  "security": "Security",
  "status": "Status and conditions",
  "troubleshooting": "Troubleshooting",
};
