"use strict";

var greeting = "hello from sobek";
var counter = 0;

var cnt = 69;

/**
 * @param {number} n
 * @returns {number}
 */
function fib(n) {
	if (n <= 1) return n;
	return fib(n - 1) + fib(n - 2);
}

/** @param {{name: string, value: number}} item */
function work(item) {
	var doubled = item.value * 2;
	counter = counter + doubled;
	return doubled;
}

const items = [
	{ name: "a", value: 1 },
	{ name: "b", value: 2 },
	{ name: "c", value: 3 },
];

log(greeting);

for (const item of items) {
	const d = work(item);
	log("worked", item.name, "->", d);
}

// Uncomment to force a stop without setting a breakpoint:
// debugger;

const f = fib(10);
log("fib(10) =", f, "counter =", counter);
