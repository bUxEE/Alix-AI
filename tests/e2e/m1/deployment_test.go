package m1

import (
	"sync"
	"testing"
)

// deployment is a deployment of AlixAI for the clinic application: the application running as a
// subprocess, the operator's data directory, and the clinic's signed package installed in it and
// activated by the integration audit.
type deployment struct {
	clinic *clinicApp
	op     *operator
	// pkgDir is the signed package that was installed.
	pkgDir string
	// hash is its manifest hash: the package's identity in the store.
	hash string
}

// newDeployment does what an operator does to put the clinic application into service, with the
// real commands: register the publisher and the report-signing key, install the signed package
// into quarantine, register the key the host signs its tokens with, run the integration audit,
// accept the warnings an authorised operator reviewed and run it again. It fails on any critical
// finding.
func newDeployment(t tb) *deployment {
	t.Helper()
	clinic := startClinic(t)
	op := newOperator(t)
	pkg := signedClinicPackage(t, clinic)
	inst := op.install(pkg)
	op.addIssuerKey(pkg)
	run := op.activate()
	if run.ManifestHash != inst.ManifestHash {
		t.Fatalf("the audit activated %s, the operator installed %s", run.ManifestHash, inst.ManifestHash)
	}
	return &deployment{clinic: clinic, op: op, pkgDir: pkg, hash: inst.ManifestHash}
}

// newInstalledDeployment is a deployment whose package is installed and never audited: the
// application is in quarantine, which is where every package starts, and serves nothing.
func newInstalledDeployment(t tb) *deployment {
	t.Helper()
	clinic := startClinic(t)
	op := newOperator(t)
	pkg := signedClinicPackage(t, clinic)
	inst := op.install(pkg)
	op.addIssuerKey(pkg)
	return &deployment{clinic: clinic, op: op, pkgDir: pkg, hash: inst.ManifestHash}
}

// with returns the deployment as seen by one test: its operator reports a failing command to that
// test, not to the lifecycle that built the deployment.
func (d *deployment) with(t tb) *deployment {
	c := *d
	c.op = d.op.with(t)
	return &c
}

func (o *operator) with(t tb) *operator {
	c := *o
	c.t = t
	return &c
}

var (
	sharedOnce sync.Once
	sharedDep  *deployment
	sharedErr  error
)

// sharedDeployment is the deployment the conversation tests run against: built once per test
// binary (an integration audit against the real application takes a while), and not owned by the
// test that asked first. Each test seeds a disposable tenant of its own in it.
func sharedDeployment(t *testing.T) *deployment {
	t.Helper()
	sharedOnce.Do(func() {
		sharedDep, sharedErr = guard(func() *deployment { return newDeployment(shared) })
	})
	if sharedErr != nil {
		t.Fatalf("the deployment under test could not be put into service: %v", sharedErr)
	}
	if !sharedDep.clinic.alive() {
		t.Fatal("the clinic application subprocess has exited")
	}
	return sharedDep.with(t)
}
