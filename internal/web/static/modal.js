// Закрытие модального окна клавишей Escape.
//
// Модалка построена на CSS :target и работает без JS: открытие — переход на
// #id, закрытие — переход на #id-close. Клавиатура при этом оставалась без
// привычного выхода: Escape не делал ничего, и единственным способом закрыть
// окно был крестик или фон.
//
// Улучшение прогрессивное: без JS всё работает как раньше.
(function () {
	"use strict";

	// closeAnchor находит якорь закрытия для открытой модалки.
	//
	// Открытой считается либо та, на которую указывает адресная строка (:target),
	// либо открытая сервером (класс modal--open) — вторая появляется, когда форма
	// вернулась с ошибкой валидации и не должна терять введённое.
	function closeAnchor() {
		var hash = window.location.hash;
		if (hash && hash.length > 1) {
			var targeted = document.getElementById(hash.slice(1));
			if (targeted && targeted.classList.contains("modal")) {
				return targeted.id + "-close";
			}
		}
		var served = document.querySelector(".modal.modal--open");
		return served ? served.id + "-close" : "";
	}

	document.addEventListener("keydown", function (ev) {
		if (ev.key !== "Escape" || ev.defaultPrevented) {
			return;
		}
		var anchor = closeAnchor();
		if (!anchor) {
			return;
		}
		// Тот же переход, что делает крестик: правило .modal-dismiss:target
		// перебивает и :target, и открытие с сервера.
		window.location.hash = anchor;
	});
})();
