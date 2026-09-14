import { test } from "node:test";
import assert from "node:assert/strict";
import { buildWorld, fireEvent } from "./dom.mjs";

// Спецификация: capture идёт по ВСЕЙ цепочке предков независимо от того,
// всплывает ли событие — bubbles управляет только фазой всплытия.
test("dom shim: capture-слушатель на document видит невсплывающее событие из глубины дерева", function () {
	var { document } = buildWorld();
	var wrap = document.createElement("div");
	var deep = document.createElement("input");
	wrap.appendChild(deep);
	document.body.appendChild(wrap);

	var seenByDocumentCapture = false;
	document.addEventListener("focus", function () { seenByDocumentCapture = true; }, true);

	fireEvent(deep, "focus", {}, false); // bubbles=false — как настоящий DOM focus

	assert.equal(seenByDocumentCapture, true, "capture-фаза обязана дойти до document даже без всплытия");
});

test("dom shim: невсплывающее событие не доходит до bubble-слушателя на предке", function () {
	var { document } = buildWorld();
	var wrap = document.createElement("div");
	var deep = document.createElement("input");
	wrap.appendChild(deep);
	document.body.appendChild(wrap);

	var seenByWrapBubble = false;
	wrap.addEventListener("focus", function () { seenByWrapBubble = true; });

	fireEvent(deep, "focus", {}, false);

	assert.equal(seenByWrapBubble, false, "bubble-фаза не всплывающего события не идёт дальше цели");
});
