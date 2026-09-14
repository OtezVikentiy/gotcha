/* Прогрессивное улучшение: без скрипта форма скачивает файл нативно, просто без
 * подсказки об усечении на экране — сами числа усечения всё равно лежат в файле. */
(function () {
	"use strict";
	function hideHint(form) {
		var hint = form.querySelector("[data-export-truncated]");
		if (hint) {
			hint.hidden = true;
			hint.textContent = "";
		}
	}
	function showHint(form, counts) {
		var hint = form.querySelector("[data-export-truncated]");
		if (!hint || !counts) return;
		var rowTpl = form.dataset.lExportTruncatedRow || "{table}: {returned}/{total}";
		var rows = [];
		for (var table in counts) {
			var c = counts[table];
			if (c && c.total > c.returned) {
				rows.push(
					rowTpl
						.replace("{table}", table)
						.replace("{returned}", String(c.returned))
						.replace("{total}", String(c.total))
				);
			}
		}
		if (rows.length === 0) return;
		hint.textContent = (form.dataset.lExportTruncated || "") + " " + rows.join(", ");
		hint.hidden = false;
	}
	function triggerDownload(text) {
		var blob = new Blob([text], { type: "application/json" });
		var url = URL.createObjectURL(blob);
		var a = document.createElement("a");
		a.href = url;
		a.download = "subject-export.json";
		document.body.appendChild(a);
		a.click();
		a.remove();
		URL.revokeObjectURL(url);
	}
	document.addEventListener("submit", function (ev) {
		var form = ev.target.closest ? ev.target.closest("[data-export-form]") : null;
		// exportNative — форма один раз уже прошла через фолбэк, дальше её не перехватываем.
		if (!form || form.dataset.exportNative) return;
		ev.preventDefault();
		hideHint(form);
		fetch(form.action, { method: "POST", body: new FormData(form) })
			.then(function (resp) {
				return resp.text().then(function (text) {
					return { resp: resp, text: text };
				});
			})
			.then(function (r) {
				// redirected — сессия истекла и fetch тихо ушёл за формой входа; !ok — сервер
				// ответил ошибкой. В обоих случаях тело не файл, скачивать его нельзя.
				var data = null;
				if (r.resp.ok && !r.resp.redirected) {
					try {
						data = JSON.parse(r.text);
					} catch (e) {
						// не JSON — не файл, ниже уйдёт в тот же фолбэк, что и redirected/!ok.
					}
				}
				if (!data) {
					form.dataset.exportNative = "1";
					form.submit();
					return;
				}
				if (data.truncated) showHint(form, data.counts);
				triggerDownload(r.text);
			})
			.catch(function () {
				form.dataset.exportNative = "1";
				form.submit();
			});
	});
})();
