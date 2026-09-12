// Where this documentation is published, and where Ptah's is, declared once.
//
// The operator's guide is its own site with its own versions. It carries links
// to Ptah's documentation for the subjects Ptah owns -- schema formats, OCI
// artifacts, the CLI -- and those links are built here rather than typed into
// pages, so the two sites can be related without this one depending on a build
// of the other.
//
// Plain ESM with no imports, so astro.config.mjs, the gates and the version
// generator can all read it.

// Origin is the scheme and host this documentation is served from, with no
// trailing slash.
export const Origin = 'https://operator.ptah.run';

// PtahOrigin is where Ptah's own documentation lives. Nothing here imports from
// that site or builds against it; this is the address of a link.
export const PtahOrigin = 'https://docs.ptah.run';

// BasePath is the site-root-relative prefix every page of one version lives
// under, with leading and trailing slashes.
//
// Just the version: the site is served at the apex of its own domain. It is a
// function rather than a constant because the version is chosen per build.
export function BasePath(version) {
  return `/${version}/`;
}

// RootURL is the absolute URL of a file published at the site root, beside the
// version folders rather than inside one.
export function RootURL(name) {
  return `${Origin}/${name}`;
}

// PageURL is the absolute URL of one page of one version.
export function PageURL(version, route) {
  return `${Origin}${BasePath(version)}${route}`;
}

// PtahDocsURL addresses a page of Ptah's documentation.
//
// The version is explicit because Ptah publishes one directory per version and
// a link that omitted it would land on whatever the apex redirect serves that
// day. `edge` is what a reader of the development guide wants; a released
// operator guide names the Ptah version its compatibility row records.
export function PtahDocsURL(version, route) {
  return `${PtahOrigin}/${version}/${route}`;
}

// CompatibilityURL is the permanent address of the compatibility matrix on
// Ptah's site. It sits outside the per-version archives on purpose: the matrix
// is current, and a copy frozen into a CLI release would answer with whatever
// was true the day that CLI shipped.
export const CompatibilityURL = `${PtahOrigin}/compatibility/operator/`;
