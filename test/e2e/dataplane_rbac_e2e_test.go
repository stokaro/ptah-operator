//go:build e2e

package e2e

func (d *dataPlane) pauseStatusWrites() {
	d.t.Helper()
	if d.rbac.paused {
		d.fatalf("controller status-write RBAC is already paused")
	}
	d.rbac.cluster = d.cluster
	d.rbac.role = d.controllerName
	d.rbac.user = "system:serviceaccount:" + d.in.OperatorNamespace + ":" + d.controllerServiceAccount
	d.rbac.namespace = d.in.TestNamespace
	d.rbac.resource = "ptahschemas"
	d.check(d.rbac.pause(d.ctx), "pause controller status writes")
}

func (d *dataPlane) resumeStatusWrites() error {
	return d.rbac.resume(d.ctx)
}

func (d *dataPlane) mustResumeStatusWrites(reason string) {
	d.t.Helper()
	if err := d.resumeStatusWrites(); err != nil {
		d.fatalf("%s: %v", reason, err)
	}
}
