// Host API surface for debugged scripts.
//
// `log` is the only binding the demo host provides today. `db`, `ui`, and
// `jobs` are the planned surface for the database app, declared now so
// scripts get IntelliSense and checking while the API is designed. Keep
// this file in lockstep with the Go side: a declaration with no backing
// binding is a ReferenceError at runtime, not a type error.
//
// All host calls are synchronous — blocking happens on the script's Go
// goroutine, never via promises. Nothing here returns a Promise by design.
//
// (`__dbg` and `__x` are reserved by the instrumenter; never use them.)

/** Print values to the host's stdout log. */
declare function log(...args: unknown[]): void;

/** Block the script for `ms` milliseconds (demo of a blocking host call). */
declare function sleep(ms: number): void;

/**
 * Run another script file (resolved next to the current one) in the same VM.
 * Globals are shared; the sub-script's completion value is returned and its
 * errors surface here as catchable exceptions.
 */
declare function call(script: string): unknown;

/** One record: column name → value. */
type Row = Record<string, unknown>;

interface FieldOpts {
	type: "text" | "number" | "date" | "bool";
	required?: boolean;
	unique?: boolean;
	/** Default value applied when a record is created without one. */
	default?: unknown;
}

/**
 * Database operations. Available in every script context.
 * Failures throw; there is no error-code channel to poll.
 */
declare const db: {
	/** Return all records in `table` matching `query` (equality on each key). */
	find(table: string, query: Row): Row[];
	createField(table: string, name: string, opts: FieldOpts): void;
	/** Write a consistent snapshot of the database to `path` on the server. */
	backup(path: string): void;
};

/** A widget on the layout the triggering user is viewing. */
interface Widget {
	/** Read or assign the widget's current value. */
	value: unknown;
	focus(): void;
}

/**
 * The triggering user's browser session. Session scripts only — job
 * scripts have no attached client, and every `ui` call there throws.
 */
declare const ui: {
	/** Look up a widget by its id on the current layout. Throws if absent. */
	widget(id: string): Widget;
	/**
	 * Show a modal dialog and block until dismissed. Returns the label of
	 * the button pressed. Default buttons: ["OK"].
	 */
	showDialog(message: string, buttons?: string[]): string;
	goToLayout(name: string): void;
};

/** A session-detached script run started with `jobs.start`. */
interface Job {
	readonly id: string;
	/**
	 * Block until the job finishes and return its result; rethrows the
	 * job's error if it failed. Throws on timeout.
	 */
	wait(timeoutMs?: number): unknown;
}

/**
 * Detached execution: the job runs in its own fresh VM with no `ui`.
 * Fire-and-forget by ignoring the returned handle.
 */
declare const jobs: {
	start(script: string, params?: Row): Job;
};
