"use strict";

log("callee: start");
var subTotal = 0;
for (var s = 1; s <= 3; s++) {
	subTotal = subTotal + s;
}
log("callee: subTotal =", subTotal);
