"use strict";

// Exercise pausing inside a blocked host call: run with
//   ./bin/demo -wait testdata/blocking.js
// attach (F5), let it run, then hit Pause while a sleep() is in flight.
// The debugger should stop on the sleep line with variables readable and
// writable; Continue returns the script to its sleep.

var ticks = 0;

log("each loop sleeps 3s — press Pause (F6) mid-sleep");

for (var i = 1; i <= 100; i++) {
	sleep(3000);
	ticks = ticks + 1;
	log("tick", ticks);
}
