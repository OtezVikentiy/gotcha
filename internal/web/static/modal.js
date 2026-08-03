// Клавиатурное поведение модалки: Escape, фокус при открытии, возврат фокуса
// при закрытии.
//
// Модалка построена на CSS :target и работает без JS: открытие — переход на
// #id, закрытие — переход на #id-close. Без JS так и остаётся. Улучшения
// прогрессивные (№80, №81):
//  - Escape закрывает открытую модалку (в т.ч. открытую сервером);
//  - при открытии фокус уходит на заголовок диалога (tabindex=-1), при
//    серверном переоткрытии после ошибки валидации — тоже, иначе клавиатура
//    остаётся в начале страницы и модалку «не видит»;
//  - при закрытии фокус возвращается на элемент, с которого открывали
//    (образец — close(refocus) в daterange.js).
(function () {
	"use strict";

	var opener = null;

	function modalFromHash() {
		var hash = window.location.hash;
		if (!hash || hash.length < 2) {
			return null;
		}
		var el = document.getElementById(hash.slice(1));
		return el && el.classList.contains("modal") ? el : null;
	}

	function focusHeading(modal) {
		var h = modal.querySelector(".modal-card > .card-header");
		if (h) {
			h.focus();
		}
	}

	// closeAnchor находит якорь закрытия для открытой модалки: либо той, на
	// которую указывает адресная строка (:target), либо открытой сервером
	// (класс modal--open — форма вернулась с ошибкой валидации).
	function closeAnchor() {
		var targeted = modalFromHash();
		if (targeted) {
			return targeted.id + "-close";
		}
		var served = document.querySelector(".modal.modal--open");
		return served ? served.id + "-close" : "";
	}

	// Открыватель запоминается на click (capture), ДО навигации по якорю:
	// к моменту hashchange браузер уже обработал фрагмент и увёл фокус с
	// триггера, activeElement там — body. Enter на ссылке тоже даёт click.
	document.addEventListener("click", function (ev) {
		var a = ev.target.closest ? ev.target.closest('a[href^="#"]') : null;
		if (!a) {
			return;
		}
		var target = document.getElementById(a.getAttribute("href").slice(1));
		if (target && target.classList.contains("modal")) {
			opener = a;
		}
	}, true);

	window.addEventListener("hashchange", function () {
		var m = modalFromHash();
		if (m) {
			focusHeading(m);
		} else if (opener) {
			opener.focus();
			opener = null;
		}
	});

	document.addEventListener("keydown", function (ev) {
		if (ev.key !== "Escape" || ev.defaultPrevented) {
			return;
		}
		var anchor = closeAnchor();
		if (!anchor) {
			return;
		}
		// Тот же переход, что делает крестик: правило .modal-dismiss:target
		// перебивает и :target, и открытие с сервера. Возврат фокуса сделает
		// обработчик hashchange выше.
		window.location.hash = anchor;
	});

	// Серверное переоткрытие: страница загрузилась с .modal.modal--open —
	// увести фокус в диалог сразу (opener пуст: возвращать некуда, фокус
	// после закрытия останется на закрывшем элементе).
	var served = document.querySelector(".modal.modal--open");
	if (served) {
		focusHeading(served);
	}
})();
