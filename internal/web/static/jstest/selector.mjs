// Мини-движок CSS-селекторов ровно под подмножество, нужное static/*.js:
// тег/.class/#id/[attr]/:not(), списки через запятую, потомок и прямой потомок.

var SUPPORTED_OPS = ["=", "^="];

// Инструмент, который не умеет часть синтаксиса, обязан бросить, а не молча
// подобрать по одному наличию атрибута — иначе тест зеленеет на чужих узлах.
function parseCompound(token) {
	var compound = { tag: null, id: null, classes: [], attrs: [], nots: [] };
	var rest = token;
	var tagMatch = rest.match(/^[a-zA-Z][a-zA-Z0-9-]*/);
	if (tagMatch) {
		compound.tag = tagMatch[0].toLowerCase();
		rest = rest.slice(tagMatch[0].length);
	}
	var re = /\.[a-zA-Z0-9_-]+|#[a-zA-Z0-9_-]+|\[[a-zA-Z0-9_-]+(?:([~^$*]?=)"([^"]*)")?\]|:not\(([^)]*)\)/g;
	var pos = 0;
	var m;
	while ((m = re.exec(rest))) {
		if (m.index !== pos) {
			throw new Error('selector.mjs: неподдерживаемый синтаксис в "' + token + '" на позиции ' + pos);
		}
		pos = re.lastIndex;
		var t = m[0];
		if (t[0] === ".") {
			compound.classes.push(t.slice(1));
		} else if (t[0] === "#") {
			compound.id = t.slice(1);
		} else if (t.indexOf(":not(") === 0) {
			compound.nots.push(parseCompound(m[3]));
		} else {
			var am = t.match(/^\[([a-zA-Z0-9_-]+)(?:([~^$*]?=)"([^"]*)")?\]/);
			var op = am[2] || null;
			if (op && SUPPORTED_OPS.indexOf(op) < 0) {
				throw new Error('selector.mjs: оператор атрибута "' + op + '" не поддержан ("' + token + '")');
			}
			compound.attrs.push({ name: am[1], op: op, value: am[3] });
		}
	}
	if (pos !== rest.length) {
		throw new Error('selector.mjs: неподдерживаемый синтаксис в "' + token + '" на позиции ' + pos);
	}
	return compound;
}

function parseSelector(selector) {
	var tokens = selector.trim().split(/\s+/);
	var steps = [];
	var combinator = "descendant";
	for (var i = 0; i < tokens.length; i++) {
		if (tokens[i] === ">") {
			combinator = "child";
			continue;
		}
		steps.push({ combinator: combinator, compound: parseCompound(tokens[i]) });
		combinator = "descendant";
	}
	return steps;
}

function parseSelectorList(selector) {
	return selector.split(",").map(function (s) {
		return parseSelector(s.trim());
	});
}

function compoundMatches(el, compound) {
	if (compound.tag && el.tagName.toLowerCase() !== compound.tag) {
		return false;
	}
	if (compound.id && el.id !== compound.id) {
		return false;
	}
	for (var i = 0; i < compound.classes.length; i++) {
		if (!el.classList.contains(compound.classes[i])) {
			return false;
		}
	}
	for (var j = 0; j < compound.attrs.length; j++) {
		var a = compound.attrs[j];
		if (!el.hasAttribute(a.name)) {
			return false;
		}
		if (a.op) {
			var v = el.getAttribute(a.name) || "";
			if (a.op === "=" && v !== a.value) {
				return false;
			}
			if (a.op === "^=" && v.indexOf(a.value) !== 0) {
				return false;
			}
		}
	}
	for (var k = 0; k < compound.nots.length; k++) {
		if (compoundMatches(el, compound.nots[k])) {
			return false;
		}
	}
	return true;
}

function matchesSteps(el, steps, idx) {
	if (!el || !compoundMatches(el, steps[idx].compound)) {
		return false;
	}
	if (idx === 0) {
		return true;
	}
	if (steps[idx].combinator === "child") {
		return matchesSteps(el.parentNode, steps, idx - 1);
	}
	var anc = el.parentNode;
	while (anc) {
		if (matchesSteps(anc, steps, idx - 1)) {
			return true;
		}
		anc = anc.parentNode;
	}
	return false;
}

export function elementMatches(el, selector) {
	var lists = parseSelectorList(selector);
	for (var i = 0; i < lists.length; i++) {
		var steps = lists[i];
		if (matchesSteps(el, steps, steps.length - 1)) {
			return true;
		}
	}
	return false;
}

export function findAll(root, selector, out) {
	out = out || [];
	for (var i = 0; i < root.children.length; i++) {
		var child = root.children[i];
		if (elementMatches(child, selector)) {
			out.push(child);
		}
		findAll(child, selector, out);
	}
	return out;
}

export function findFirst(root, selector) {
	for (var i = 0; i < root.children.length; i++) {
		var child = root.children[i];
		if (elementMatches(child, selector)) {
			return child;
		}
		var found = findFirst(child, selector);
		if (found) {
			return found;
		}
	}
	return null;
}
