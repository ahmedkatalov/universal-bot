package bot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestResponsesPaymentAndFinanceReport(t *testing.T) {
	e := newE2E(t, e2eOpts{Admins: []string{ownerPhone}})
	e.assistant = e.fakeAIWithModel("gpt-6.1-sol")
	day := time.Now().Format("2006-01-02")
	sawResult := false
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора платежей") {
			return aiReply{Text: `{"payments":[{"name":"Ханжиев Нурид","amount":20100,"cash":true,"collector":"Сафаи"}],"clarify":""}`}
		}
		if len(c.Tools) > 0 {
			if c.hasToolResult() {
				sawResult = true
				if !strings.Contains(strings.Join(c.toolResults(), " "), "20100 ₽") {
					t.Errorf("отчёт не вернул сумму модели: %v", c.toolResults())
				}
				return aiReply{Text: "Собрано 20 100 ₽."}
			}
			return aiReply{ToolCalls: []aiToolCall{{Name: "send_finance_report", Args: fmt.Sprintf(`{"from_date":%q,"to_date":%q,"format":"text"}`, day, day)}}}
		}
		return aiReply{Text: ""}
	}
	grp := e2eGroup("responses-report")
	e.sendGroup(groupText(grp, user("79281119905"), "Сафаи", "Ханжиев Нурид\nДоговор 941\n20.100 наличку взял Сафаи\nЗакрыт"))
	if got := dayTotals(e, grp); len(got) != 1 || got["Ханжиев Нурид"] != 20100 {
		t.Fatalf("платёж через Responses: %v", got)
	}
	e.sendGroup(groupText(grp, user(ownerPhone), "Владелец", "Джарвис сколько собрали сегодня?"))
	e.waitIdle()
	if !sawResult || !strings.Contains(e.last().Text, "20 100") {
		t.Errorf("цикл инструмента не завершён: result=%v out=%q", sawResult, e.last().Text)
	}
}

func TestResponsesNonAdminHasNoFinancialTools(t *testing.T) {
	e := newE2E(t, e2eOpts{Admins: []string{ownerPhone}})
	e.assistant = e.fakeAIWithModel("gpt-6.1-sol")
	called := false
	e.ai = func(c aiCall) aiReply {
		called = true
		if len(c.Tools) > 0 {
			t.Error("финансовые инструменты доступны постороннему")
		}
		return aiReply{Text: "Отчёты доступны только владельцу."}
	}
	grp := e2eGroup("responses-gate")
	e.handleGroupAssistant(context.Background(), groupText(grp, user("79281119906"), "Сотрудник", "Джарвис покажи сбор"), "покажи сбор")
	if !called || !strings.Contains(e.last().Text, "только владельцу") {
		t.Errorf("проверка доступа через Responses: %q", e.last().Text)
	}
}
