package cli

// The RAG checks of the integration audit (master plan 4.5 check 3, live half, and the
// RAG parts of checks 6 and 10) register themselves with the audit's default registry from
// an init function of their own package, and nothing else imports that package into the
// binary. Importing it here, in the composition root, is what makes the binary run them:
// without this line the audit says nothing about undeclared and unreachable sources,
// chunk-level scope, citations or the RAG path of the smoke test, and every activation needs
// the warnings for those sections accepted.
//
// What the checks look at beyond the package (the sources the deployment has registered, the
// operator's mounts) is supplied through rag.SetDeploymentSource. Until a composition root
// supplies it, they record what they could not look at as unverified, never as clean.
import _ "github.com/buxee/alix-ai/internal/integrationaudit/rag"
