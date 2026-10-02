package cmf

import (
	"errors"
	"fmt"
)

// APIError — ошибка обращения к программе рассрочек с классифицируемой причиной.
// Раньше бот получал голую строку и на каждый чек писал «ошибка поиска», не
// говоря, ЧТО именно сломалось (адрес? пароль? сеть?). Теперь причину видно.
type APIError struct {
	Op     string // что делали: "вход", "поиск клиента", "договоры", "платежи", "внесение"
	Status int    // HTTP-код; 0 — до ответа не дошло (сеть/таймаут/DNS)
	Body   string // фрагмент ответа программы (только в лог)
	Err    error  // сетевая/прочая ошибка
}

func (e *APIError) Error() string {
	switch {
	case e.Status == 0 && e.Err != nil:
		return fmt.Sprintf("cmf %s: %v", e.Op, e.Err)
	case e.Body != "":
		return fmt.Sprintf("cmf %s: код %d: %s", e.Op, e.Status, e.Body)
	default:
		return fmt.Sprintf("cmf %s: код %d", e.Op, e.Status)
	}
}

func (e *APIError) Unwrap() error { return e.Err }

// Transient — временный сбой, который имеет смысл повторить (сеть, перегрузка,
// ошибка на стороне программы). Неверный адрес/пароль повторять бесполезно.
func (e *APIError) Transient() bool {
	return e.Status == 0 || e.Status == 429 || e.Status >= 500
}

// Human — короткая причина простыми словами для владельца (без секретов).
func Human(err error) string {
	var ae *APIError
	if !errors.As(err, &ae) {
		if err == nil {
			return ""
		}
		return "программа не ответила"
	}
	switch {
	case ae.Status == 0:
		return "программа не отвечает по сети (сервер недоступен или неверный адрес CMF_API_URL)"
	case ae.Op == "вход" && (ae.Status == 401 || ae.Status == 403 || ae.Status == 400 || ae.Status == 422):
		return "программа не пускает: неверный логин или пароль бота (CMF_EMAIL / CMF_PASSWORD)"
	case ae.Op == "вход" && ae.Status == 200:
		return "вход прошёл, но программа не выдала токен — похоже, CMF_API_URL указывает не на тот сервис"
	case ae.Status == 401 || ae.Status == 403:
		return "программа отказала в доступе — у пользователя бота нет прав на клиентов/договоры"
	case ae.Status == 404:
		return "неверный адрес программы: путь не найден (проверь CMF_API_URL — без лишнего «/api» в конце)"
	case ae.Status == 429:
		return "программа ограничила частоту запросов — повтори через минуту"
	case ae.Status >= 500:
		return fmt.Sprintf("программа сейчас сбоит на своей стороне (ошибка %d)", ae.Status)
	default:
		return fmt.Sprintf("программа вернула ошибку %d", ae.Status)
	}
}

// IsTransient — для вызывающего кода (вне пакета).
func IsTransient(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Transient()
}
