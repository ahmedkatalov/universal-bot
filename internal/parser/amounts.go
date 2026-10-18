package parser

// AmountAt — сумма в тексте и где стоит её число.
type AmountAt struct {
	Value      float64
	Start, End int // байты числа в тексте (без суффикса «т», «тыс», «руб»)
	// Real — похоже на деньги: с суффиксом тысяч или от 100 ₽, не телефон и не
	// номер договора («за 2 месяца» — не сумма платежа).
	Real bool
}

// AmountsAt — все суммы текста по порядку, как их видит ExtractAmount (без
// дат «18.10» и времени «12:30»): чтобы понять, сколько в строке платежей и
// чьё ФИО стоит перед каждой суммой.
func AmountsAt(text string) []AmountAt {
	hits := amountHits(text)
	out := make([]AmountAt, 0, len(hits))
	for _, h := range hits {
		out = append(out, AmountAt{Value: h.v, Start: h.start, End: h.end, Real: h.real})
	}
	return out
}
