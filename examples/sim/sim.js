"use strict";

// Simulation script for the tick-loop example.
//
// The host owns the clock and the screen; this file owns the state. It runs
// once to define `world` and `tick`, and after that the host calls tick()
// about eight times a second and draws whatever `world` says.

var world = {
	tick: 0,
	width: 46,
	height: 12,
	entities: [
		// A starts nearly dead on purpose — it stops moving within a few
		// seconds, which is your cue to revive it from the Debug Console.
		{ name: "A", x: 3, y: 2, dx: 1, dy: 1, hp: 8 },
		{ name: "B", x: 30, y: 8, dx: -1, dy: 1, hp: 220 },
		{ name: "C", x: 18, y: 5, dx: 1, dy: -1, hp: 60 },
	],
};

/** Advance the world by one step. Called by the host, once per tick. */
function tick() {
	world.tick = world.tick + 1;
	for (var i = 0; i < world.entities.length; i++) {
		step(world.entities[i]);
	}
}

/**
 * Move one entity and age it by a tick.
 * @param {{name: string, x: number, y: number, dx: number, dy: number, hp: number}} e
 */
function step(e) {
	// Dead entities don't move. Set e.hp above zero in the Debug Console
	// while paused and this guard stops rejecting it — the entity starts
	// moving again on the next frame.
	if (e.hp <= 0) {
		return;
	}

	e.hp = e.hp - 1;
	if (e.hp === 0) {
		log("tick " + world.tick + ": " + e.name + " died at (" + e.x + "," + e.y + ")");
	}

	e.x = e.x + e.dx;
	e.y = e.y + e.dy;

	// Bounce off the walls. Good line for a conditional breakpoint —
	// `e.name === "C"` catches one entity's bounces and ignores the rest.
	if (e.x <= 0 || e.x >= world.width - 1) {
		e.dx = -e.dx;
		e.x = e.x + e.dx;
	}
	if (e.y <= 0 || e.y >= world.height - 1) {
		e.dy = -e.dy;
		e.y = e.y + e.dy;
	}
}

log("simulation ready: " + world.entities.length + " entities");
