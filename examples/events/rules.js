"use strict";

// Handler script for the event-host example.
//
// This file runs exactly once, at startup, and all it does is register
// handlers. Everything below the on(...) calls happens later, once per
// matching event, driven by the Go host.

// Running totals live at file scope so they survive between events — pause
// inside a handler and they're in the Variables pane, accumulating.
var ordersSeen = 0;
var revenue = 0;
const flagged = [];

on("order.created", function (event) {
	ordersSeen = ordersSeen + 1;
	const value = event.total * event.items;
	revenue = revenue + event.total;
	log("order", event.id, "value", value.toFixed(2));

	// Good line for a *conditional* breakpoint: `event.total > 1000` stops
	// only on the one order that trips it, with ordersSeen/revenue already
	// carrying the state from every event before it.
	if (event.total > 1000) {
		flagged.push(event.id);
		log("flagged for review:", event.id);
	}
});

// A second handler for the same event type: both run, in registration order.
on("order.created", function (event) {
	if (event.customer === "acme") {
		log("  (acme order " + event.id + ")");
	}
});

on("order.refunded", function (event) {
	// Deliberately buggy: one refund in events.json has no `reason`, so this
	// throws. The host reports the failure and keeps feeding, so a breakpoint
	// here lands you on the bad event with every earlier one already applied
	// — which is the situation the debugger is actually for.
	const reason = event.reason.trim();
	revenue = revenue - event.total;
	log("refund", event.id, "-", reason);
});

log("rules registered");
