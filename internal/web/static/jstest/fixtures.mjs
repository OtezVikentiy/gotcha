// Фикстуры повторяют структуру modal.templ/logs.templ ровно настолько, насколько
// нужно modal.js/logs.js — без разбора HTML, узлы собираются напрямую через DOM API.

export function buildModal(doc, id, opts) {
	opts = opts || {};
	var dismiss = doc.createElement("span");
	dismiss.setAttribute("id", id + "-close");
	dismiss.classList.add("modal-dismiss");

	var modal = doc.createElement("div");
	modal.setAttribute("id", id);
	modal.classList.add("modal");
	if (opts.open) {
		modal.classList.add("modal--open");
	}

	var card = doc.createElement("div");
	card.classList.add("modal-card");

	var closeLink = doc.createElement("a");
	closeLink.classList.add("modal-close");
	closeLink.setAttribute("href", "#" + id + "-close");

	var header = doc.createElement("h2");
	header.classList.add("card-header");
	header.tabIndex = -1;

	var field = doc.createElement("input");
	field.tabIndex = 0;

	card.appendChild(closeLink);
	card.appendChild(header);
	card.appendChild(field);
	modal.appendChild(card);

	doc.body.appendChild(dismiss);
	doc.body.appendChild(modal);

	return { dismiss: dismiss, modal: modal, card: card, closeLink: closeLink, header: header, field: field };
}

export function buildOpenerLink(doc, targetID) {
	var a = doc.createElement("a");
	a.setAttribute("href", "#" + targetID);
	doc.body.appendChild(a);
	return a;
}

export function buildCopyWidget(doc) {
	var root = doc.createElement("div");
	root.classList.add("copy-llm");

	var ta = doc.createElement("textarea");
	ta.setAttribute("id", "ta1");
	ta.value = "payload";
	ta.setAttribute("aria-hidden", "true");

	var done = doc.createElement("span");
	done.setAttribute("data-copy-done", "");
	done.setAttribute("role", "status");
	done.setAttribute("aria-live", "polite");
	done.hidden = true;

	var failed = doc.createElement("span");
	failed.setAttribute("data-copy-failed", "");
	failed.setAttribute("role", "alert");
	failed.hidden = true;

	var btn = doc.createElement("button");
	btn.setAttribute("data-copy-format", "md");
	btn.setAttribute("data-copy-target", "ta1");

	root.appendChild(ta);
	root.appendChild(done);
	root.appendChild(failed);
	root.appendChild(btn);
	doc.body.appendChild(root);

	return { root: root, textarea: ta, done: done, failed: failed, button: btn };
}

export function buildExportForm(doc) {
	var form = doc.createElement("form");
	form.setAttribute("data-export-form", "");
	form.setAttribute("data-l-export-truncated", "Усечено:");
	form.setAttribute("data-l-export-truncated-row", "{table}: {returned}/{total}");
	form.action = "/export";

	var hint = doc.createElement("p");
	hint.setAttribute("data-export-truncated", "");
	hint.hidden = true;

	form.appendChild(hint);
	doc.body.appendChild(form);

	return { form: form, hint: hint };
}

export function buildTimeRange(doc, opts) {
	opts = opts || {};
	var form = doc.createElement("form");

	var root = doc.createElement("div");
	root.classList.add("time-range");

	var select = doc.createElement("select");
	select.setAttribute("name", "period");
	(opts.periods || [
		{ value: "1h", label: "Последний час" },
		{ value: "24h", label: "Последние 24 часа" },
		{ value: "custom", label: "Свой диапазон" },
	]).forEach(function (p) {
		var o = doc.createElement("option");
		o.setAttribute("value", p.value);
		o.textContent = p.label;
		select.appendChild(o);
	});
	select.selectedIndex = opts.selectedIndex != null ? opts.selectedIndex : 0;

	var start = doc.createElement("input");
	start.setAttribute("name", "start");
	var end = doc.createElement("input");
	end.setAttribute("name", "end");

	root.appendChild(select);
	root.appendChild(start);
	root.appendChild(end);

	var cStart, cEnd;
	if (opts.custom) {
		cStart = doc.createElement("input");
		cStart.setAttribute("type", "hidden");
		cStart.setAttribute("name", "cstart");
		cStart.value = opts.custom.start;
		cEnd = doc.createElement("input");
		cEnd.setAttribute("type", "hidden");
		cEnd.setAttribute("name", "cend");
		cEnd.value = opts.custom.end;
		root.appendChild(cStart);
		root.appendChild(cEnd);
	}

	form.appendChild(root);
	doc.body.appendChild(form);

	return { form: form, root: root, select: select, start: start, end: end, cStart: cStart, cEnd: cEnd };
}

export function buildTypeahead(doc, opts) {
	opts = opts || {};
	var root = doc.createElement("div");
	root.setAttribute("data-attr-typeahead", "");
	root.setAttribute("data-attr-keys-url", opts.keysURL || "/keys");
	root.setAttribute("data-attr-base-href", opts.baseHref || "/projects/705/logs");

	var input = doc.createElement("input");
	input.setAttribute("data-attr-typeahead-input", "");

	var list = doc.createElement("ul");
	list.setAttribute("data-attr-typeahead-list", "");
	list.hidden = true;

	root.appendChild(input);
	root.appendChild(list);
	doc.body.appendChild(root);

	return { root: root, input: input, list: list };
}
