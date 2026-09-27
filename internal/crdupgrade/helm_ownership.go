package crdupgrade

// The metadata Helm writes on every object of a release, which is how the CRD
// hook tells the release's own runtime Deployments from anything else.
const (
	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
	managedByLabel                 = "app.kubernetes.io/managed-by"
	instanceLabel                  = "app.kubernetes.io/instance"
)
