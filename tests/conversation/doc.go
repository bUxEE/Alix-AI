// Package conversation holds the conversation tests of master plan 22.3, the corpus that
// mirrors them for the benchmark (17.3) and the world both are written against.
//
// # The tests
//
// Fourteen tests, one per bullet of 22.3, named TestConv<Behaviour> (direct commands,
// missing details, multiple details in one turn, ambiguous entities, corrections, relative
// references, cancel and restart, topic switch and resume, stale application data, denied
// capabilities, required step-up, natural responses with exact deterministic facts,
// out-of-application questions, prompt injection). Each serves real conversations from the
// clinic-booking reference application: its signed package (capabilities, field
// definitions, ambiguity rules, terminology, conversation contracts and host policy)
// loaded by internal/apppkg, an in-process stand-in for the application's behaviour
// behind adapters.Wrap, the real policy gateway, action coordinator, session store and
// dialogue engine, and a scripted model provider. The model only ever proposes; what the
// engine does with a proposal is what is under test, and every assertion is made on the
// engine's result, the application's own record of what it received and did, and the
// coordinator's and the session's stored state, never on the script.
//
// # The world
//
// ClinicHost (clinic_host.go) is the stand-in for the application: a transcription of its
// roles, record-level visibility, resolvers, business rules, fact names, snapshot versions
// and idempotent commit, whose every reply is held to the result schemas the signed package
// declares (TestClinicHostRepliesMatchThePackagesOwnSchemas). It is exported so that the
// harness that scores live models on the corpus runs the same world. Its seed is world
// "clinic-synthetic-v1"; the record identifiers in the corpus belong to it.
//
// The clinic ships no level 3 capability, so the step-up cases use the variant
// "contact-step-up": the same package under a deployment policy that raises
// patients.update_contact to level 3 (the only thing a host policy can do to a risk
// level), signed again with the TEST-ONLY publisher key.
//
// # The corpus
//
// tests/performance/corpus/conversation-v1.jsonl is described on Case. It is generated
// from the Go values in corpus_cases_test.go and is checked three ways: it is a valid
// corpus (LoadCorpus, which tools can use: this file imports only the standard library),
// it agrees with the signed package (each case's catalogue is what the policy gateway
// shows its role; each contract example it cites exists), and every case is replayed
// through the real engine with the scripted provider (TestCorpusReplays). To change it:
//
//	CONVERSATION_WRITE_CORPUS=1 go test ./tests/conversation -run TestCorpusIsCurrent
//
// and, because published corpora are never edited in place, change its id too once it has
// been used for a recorded result.
//
// # A cancellation reaches its confirmation
//
// TestConvCancelConfirmationRenders holds the path of a cancellation: the confirmation is
// rendered (it once was not, because internal/verify read the host's appointment_status =
// "cancelled" as a completion claim; fixed in M1-28), nothing is committed before the user
// confirms, and then the appointment is cancelled and its slot released.
package conversation
