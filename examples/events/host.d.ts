// Host API added by the event-host example, on top of the base surface in
// types/host.d.ts. Same rule as there: keep it in lockstep with the Go
// bindings by hand — a declaration with no backing binding is a
// ReferenceError at runtime, not a type error.

/** One event from the host's feed. `type` selects the handlers. */
interface HostEvent {
	type: string;
	[field: string]: unknown;
}

/**
 * Register a handler for events of the given type. Call at load time only:
 * the host runs this script once to collect handlers, then calls them as
 * events arrive. Multiple handlers per type run in registration order, and
 * one that throws doesn't stop the others.
 */
declare function on(type: string, handler: (event: any) => void): void;
