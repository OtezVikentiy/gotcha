// Ручной DOM-шим для прогона static/*.js под node:test через vm.createContext —
// без jsdom и без сборки: файлы гоняются как есть, тем же кодом, что летит в браузер.
import fs from "node:fs";
import vm from "node:vm";
import { elementMatches, findAll, findFirst } from "./selector.mjs";

class GEventTarget {
	constructor() {
		this._listeners = {};
	}
	addEventListener(type, fn, opts) {
		var capture = opts === true || (opts && opts.capture);
		this._listeners[type] = this._listeners[type] || [];
		this._listeners[type].push({ fn: fn, capture: !!capture });
	}
	removeEventListener(type, fn) {
		var list = this._listeners[type] || [];
		this._listeners[type] = list.filter(function (l) { return l.fn !== fn; });
	}
	_invoke(type, capture, event) {
		var list = (this._listeners[type] || []).slice();
		for (var i = 0; i < list.length; i++) {
			if (list[i].capture === capture && !event._stopped) {
				list[i].fn.call(this, event);
			}
		}
	}
}

function camelToDataAttr(camel) {
	return "data-" + String(camel).replace(/[A-Z]/g, function (c) { return "-" + c.toLowerCase(); });
}

function pathToRoot(node) {
	var path = [];
	while (node) {
		path.push(node);
		node = node.parentNode || null;
	}
	return path;
}

function dispatch(target, event, bubbles) {
	event.target = target;
	// Capture идёт по всей цепочке предков ВСЕГДА — bubbles решает только всплытие.
	var fullPath = pathToRoot(target);
	for (var i = fullPath.length - 1; i >= 0; i--) {
		fullPath[i]._invoke(event.type, true, event);
		if (event._stopped) {
			return event;
		}
	}
	var bubblePath = bubbles ? fullPath : [target];
	for (var j = 0; j < bubblePath.length; j++) {
		bubblePath[j]._invoke(event.type, false, event);
		if (event._stopped) {
			break;
		}
	}
	return event;
}

function makeEvent(type, props) {
	var e = Object.assign({ type: type, defaultPrevented: false, _stopped: false }, props || {});
	e.preventDefault = function () { e.defaultPrevented = true; };
	e.stopPropagation = function () { e._stopped = true; };
	return e;
}

class ClassList {
	constructor(el) {
		this._el = el;
		this._set = new Set();
	}
	add(c) { this._set.add(c); this._sync(); }
	remove(c) { this._set.delete(c); this._sync(); }
	toggle(c) { this._set.has(c) ? this._set.delete(c) : this._set.add(c); this._sync(); return this._set.has(c); }
	contains(c) { return this._set.has(c); }
	_sync() { this._el._attrs.class = Array.from(this._set).join(" "); }
	_fromAttr(v) {
		this._set = new Set((v || "").split(/\s+/).filter(Boolean));
	}
}

export class Element extends GEventTarget {
	constructor(tag, doc) {
		super();
		this.tagName = tag.toUpperCase();
		this._doc = doc;
		this._attrs = {};
		this.children = [];
		this.parentNode = null;
		this.classList = new ClassList(this);
		this.hidden = false;
		this._visible = true;
		this._text = "";
	}
	get firstChild() { return this.children[0] || null; }
	get id() { return this._attrs.id || ""; }
	set id(v) { this.setAttribute("id", v); }
	get className() { return this._attrs.class || ""; }
	set className(v) { this.setAttribute("class", v); }
	// Отражают HTML-атрибут в обе стороны, как в настоящем DOM — иначе селекторы
	// вида [tabindex] и код вида el.disabled разойдутся с тем, что реально задал тест.
	get tabIndex() {
		if (this.hasAttribute("tabindex")) {
			return parseInt(this.getAttribute("tabindex"), 10);
		}
		if (this.tagName === "A" || this.tagName === "AREA") {
			return this.hasAttribute("href") ? 0 : -1;
		}
		if (["BUTTON", "INPUT", "SELECT", "TEXTAREA"].indexOf(this.tagName) >= 0) {
			return 0;
		}
		return -1;
	}
	set tabIndex(v) { this.setAttribute("tabindex", String(v)); }
	get type() { return this.getAttribute("type") || ""; }
	set type(v) { this.setAttribute("type", v); }
	get disabled() { return this.hasAttribute("disabled"); }
	set disabled(v) {
		if (v) { this.setAttribute("disabled", ""); } else { this.removeAttribute("disabled"); }
	}
	// select/option — узкий срез настоящего HTMLSelectElement, ровно под то, что
	// использует enhance() в daterange.js (перебор options, чтение/запись value).
	get options() { return this.children; }
	get selectedIndex() {
		for (var i = 0; i < this.children.length; i++) {
			if (this.children[i]._selected) { return i; }
		}
		return this.tagName === "SELECT" ? -1 : undefined;
	}
	set selectedIndex(i) {
		this.children.forEach(function (o, idx) { o._selected = idx === i; });
	}
	get value() {
		if (this.tagName === "SELECT") {
			var idx = this.selectedIndex;
			return idx >= 0 ? this.children[idx].value : "";
		}
		if (this.tagName === "OPTION") {
			return this.hasAttribute("value") ? this.getAttribute("value") : this.textContent;
		}
		return this._value || "";
	}
	set value(v) {
		if (this.tagName === "SELECT") {
			var found = -1;
			this.children.forEach(function (o, idx) { if (o.value === v) { found = idx; } });
			this.selectedIndex = found;
			return;
		}
		if (this.tagName === "OPTION") {
			this.setAttribute("value", v);
			return;
		}
		this._value = v;
	}
	getBoundingClientRect() {
		return { top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0 };
	}
	get offsetParent() {
		return this.getClientRects().length > 0 ? this._doc.body : null;
	}
	getAttribute(name) { return Object.prototype.hasOwnProperty.call(this._attrs, name) ? this._attrs[name] : null; }
	hasAttribute(name) { return Object.prototype.hasOwnProperty.call(this._attrs, name); }
	setAttribute(name, value) {
		this._attrs[name] = String(value);
		if (name === "class") {
			this.classList._fromAttr(value);
		}
	}
	removeAttribute(name) { delete this._attrs[name]; }
	appendChild(child) {
		child.parentNode = this;
		this.children.push(child);
		return child;
	}
	insertBefore(child, ref) {
		child.parentNode = this;
		var i = ref ? this.children.indexOf(ref) : -1;
		if (i < 0) { this.children.push(child); } else { this.children.splice(i, 0, child); }
		return child;
	}
	removeChild(child) {
		var i = this.children.indexOf(child);
		if (i >= 0) {
			this.children.splice(i, 1);
			child.parentNode = null;
			this._refocusBodyIfRemoved(child);
		}
		return child;
	}
	remove() {
		if (this.parentNode) {
			this.parentNode.removeChild(this);
		}
	}
	get innerHTML() { return this._text; }
	set innerHTML(v) {
		if (v !== "") {
			throw new Error("dom shim: innerHTML поддерживает только очистку (\"\")");
		}
		var removed = this.children;
		this.children = [];
		this._text = "";
		var doc = this._doc;
		var active = doc && doc.activeElement;
		if (active && removed.some(function (c) { return c === active || c.contains(active); })) {
			doc._setActive(doc.body);
		}
	}
	// Реальный браузер уводит фокус на body, если удалённый узел был activeElement —
	// без этого тест K43 не отличил бы "фокус потерян" от "элемент просто исчез".
	_refocusBodyIfRemoved(child) {
		var doc = this._doc;
		var active = doc && doc.activeElement;
		if (active && (active === child || child.contains(active))) {
			doc._setActive(doc.body);
		}
	}
	get textContent() { return this._text; }
	set textContent(v) {
		this.children = [];
		this._text = String(v);
	}
	contains(other) {
		var n = other;
		while (n) {
			if (n === this) {
				return true;
			}
			n = n.parentNode;
		}
		return false;
	}
	matches(selector) { return elementMatches(this, selector); }
	closest(selector) {
		var n = this;
		while (n && n.tagName) {
			if (elementMatches(n, selector)) {
				return n;
			}
			n = n.parentNode;
		}
		return null;
	}
	querySelector(selector) { return findFirst(this, selector); }
	querySelectorAll(selector) { return findAll(this, selector); }
	getClientRects() {
		var n = this;
		while (n && n.tagName) {
			if (!n._visible || n.hidden) {
				return [];
			}
			n = n.parentNode;
		}
		return this._doc && this._doc._isConnected(this) ? [{}] : [];
	}
	focus() { this._doc._setActive(this); }
	blur() { if (this._doc.activeElement === this) { this._doc._setActive(this._doc.body); } }
	select() {}
	submit() {}
	click() { dispatch(this, makeEvent("click", {}), true); }
	get dataset() {
		var el = this;
		return new Proxy({}, {
			get: function (_, prop) {
				var v = el.getAttribute(camelToDataAttr(prop));
				return v === null ? undefined : v;
			},
			set: function (_, prop, value) {
				el.setAttribute(camelToDataAttr(prop), value);
				return true;
			},
			has: function (_, prop) { return el.hasAttribute(camelToDataAttr(prop)); },
		});
	}
}

class Document extends GEventTarget {
	constructor() {
		super();
		this.documentElement = new Element("html", this);
		this.body = new Element("body", this);
		this.documentElement.appendChild(this.body);
		// document — вершина цепочки capture/bubble для document.addEventListener(type, fn, true),
		// но не участвует в подборе селекторов (у него нет tagName/classList).
		this.documentElement.parentNode = this;
		this.activeElement = this.body;
		this.readyState = "complete"; // скрипты в шаблонах — defer, DOM уже разобран к их запуску
		this.documentElement.clientWidth = 1200;
		this.execCommand = function () { return false; }; // тест переопределяет под сценарий
	}
	createElement(tag) { return new Element(tag, this); }
	createElementNS(_ns, tag) { return new Element(tag, this); }
	getElementById(id) {
		var found = null;
		(function walk(n) {
			if (found) { return; }
			for (var i = 0; i < n.children.length; i++) {
				var c = n.children[i];
				if (c.id === id) { found = c; return; }
				walk(c);
			}
		})(this.documentElement);
		return found;
	}
	querySelector(selector) { return findFirst(this.documentElement, selector); }
	querySelectorAll(selector) { return findAll(this.documentElement, selector); }
	_isConnected(el) { return this.documentElement.contains(el); }
	_setActive(el) {
		var prev = this.activeElement;
		if (prev === el) { return; }
		this.activeElement = el;
		if (prev) {
			dispatch(prev, makeEvent("blur", {}), false);
			dispatch(prev, makeEvent("focusout", { relatedTarget: el }), true);
		}
		dispatch(el, makeEvent("focus", {}), false);
		dispatch(el, makeEvent("focusin", { relatedTarget: prev }), true);
	}
}

class Location {
	constructor(win) {
		this._win = win;
		this._hash = "";
		this.origin = "http://localhost";
		this.search = "";
	}
	get hash() { return this._hash; }
	set hash(v) {
		var next = v && v[0] === "#" ? v : (v ? "#" + v : "");
		if (next === this._hash) { return; }
		this._hash = next;
		var win = this._win;
		setTimeout(function () {
			dispatch(win, makeEvent("hashchange", {}), false);
		}, 0);
	}
}

// Настоящий Node-класс FormData требует бренд HTMLFormElement и не примет наш
// Element — тут довольно факта, что new FormData(form) не бросает исключение.
class FormDataShim {
	constructor(form) { this._form = form; }
}

class Window extends GEventTarget {
	constructor() {
		super();
		this.location = new Location(this);
	}
}

export function dispatchClick(el, opts) {
	return dispatch(el, makeEvent("click", opts), true);
}

export function fireEvent(el, type, props, bubbles) {
	return dispatch(el, makeEvent(type, props), bubbles !== false);
}

export function dispatchKeydown(el, key, opts) {
	var props = Object.assign({ key: key, shiftKey: false }, opts || {});
	return dispatch(el, makeEvent("keydown", props), true);
}

export function buildWorld(extraGlobals) {
	var doc = new Document();
	var win = new Window();
	win.document = doc;
	doc.defaultView = win;
	// window === global object в реальном браузере: window.setTimeout и bare
	// setTimeout(...) — один и тот же вызов, static/*.js полагается на оба вида.
	win.setTimeout = setTimeout;
	win.clearTimeout = clearTimeout;
	win.fetch = (extraGlobals && extraGlobals.fetch) || undefined;
	win.AbortController = AbortController;
	win.getSelection = function () { return { removeAllRanges: function () {} }; };
	var sandbox = Object.assign(
		{
			document: doc,
			window: win,
			navigator: { clipboard: undefined },
			console: console,
			setTimeout: setTimeout,
			clearTimeout: clearTimeout,
			URL: URL,
			URLSearchParams: URLSearchParams,
			AbortController: AbortController,
			Blob: Blob,
			FormData: FormDataShim,
			Intl: Intl,
			Date: Date,
		},
		extraGlobals || {}
	);
	vm.createContext(sandbox);
	return { document: doc, window: win, sandbox: sandbox };
}

export function loadScript(sandbox, filePath) {
	var code = fs.readFileSync(filePath, "utf8");
	vm.runInContext(code, sandbox, { filename: filePath });
}

export function tick(ms) {
	return new Promise(function (resolve) { setTimeout(resolve, ms); });
}
