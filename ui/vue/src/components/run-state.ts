import type { ComputedRef, InjectionKey } from "vue";

// Internal, not exported by the kit: whether a run of the conversation is in flight. AlixMessageStream provides it to the cards it
// draws, so a confirmation is not answered while another run holds the thread (the controller and the gateway would refuse it).
export const RUN_IN_FLIGHT: InjectionKey<ComputedRef<boolean>> = Symbol("alixai.runInFlight");
