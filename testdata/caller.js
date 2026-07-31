"use strict";

// Script-calls-script demo: run with
//   ./bin/demo -wait testdata/caller.js
// Break on the call() line, then try: step over (callee free-runs), step
// into (lands on callee.js line 4), and step out from inside the callee
// (returns here). Globals are shared: subTotal comes back from the callee.

log("caller: before");
call("callee.js");
log("caller: after, subTotal =", subTotal);
