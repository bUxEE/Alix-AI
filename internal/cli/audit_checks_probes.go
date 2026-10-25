package cli

// The runtime and application checks of the integration audit (master plan 4.5
// checks 2, 6, 8, 9 and 10) register themselves with the audit's default registry
// from an init function of their own package, and nothing else imports that package
// into the binary. Importing it here, in the composition root, is what makes the
// binary run them: without this line the five sections stay "not run" in
// production, and every activation needs five warnings accepted.
//
// The checks reach an application's disposable tenant through the adapter the
// audit is given and a probes.Provisioner. A transport that can run the package's
// fixtures.provisioning operations registers its provisioner with
// probes.SetProvisionerSource from an init function of its own transport_<kind>.go
// file. Until one does, the checks record what they could not probe as unverified,
// and as critical for every consequential capability.
import _ "github.com/buxee/alix-ai/internal/integrationaudit/probes"
