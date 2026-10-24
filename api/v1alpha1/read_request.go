package v1alpha1

// ReadRequestAnnotation asks a PtahSchema or a PtahMigration to read its
// database now rather than at status.nextReconciliationTime, as after a
// person changed the database outside the operator. Its value is an RFC 3339
// instant, so each request differs from the last:
//
//	kubectl annotate ptahschema storefront \
//	  operator.ptah.run/read-requested-at="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite
//
// A value the status does not record as answered starts the read the
// resource would start at its next interval, once no operation is running
// and the resource is not waiting out a retry delay. It never starts a
// mutating operation, and it does not change the generation, so the current
// plan and its approval stay unless the reading retires them. A read that was
// already running when the request arrived does not answer it: it may have
// read the database before the change the request is about.
const ReadRequestAnnotation = "operator.ptah.run/read-requested-at"

// MaxReadRequestLength is the longest annotation value status records as
// given. A longer one is recorded as its SHA-256 digest, written
// "sha256:<hex>".
const MaxReadRequestLength = 64
