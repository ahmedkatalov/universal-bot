// Package report генерирует PDF-отчёт по собранным суммам: сколько всего
// собрано и в разбивке по картам/наличным.
package report

import (
	"fmt"
	"sort"

	"github.com/jung-kurt/gofpdf"
	"whatsapp-bot/internal/db"
)

// Generate строит PDF-отчёт и сохраняет его по указанному пути.
// fontDir должен содержать DejaVuSans.ttf и DejaVuSans-Bold.ttf (для кириллицы,
// т.к. встроенные шрифты gofpdf кириллицу не поддерживают).
func Generate(summaries []db.ContactSummary, periodLabel, fontDir, outPath string) error {
	pdf := gofpdf.New("P", "mm", "A4", fontDir)
	pdf.AddUTF8Font("DejaVu", "", "DejaVuSans.ttf")
	pdf.AddUTF8Font("DejaVu", "B", "DejaVuSans-Bold.ttf")
	pdf.AddPage()

	pdf.SetFont("DejaVu", "B", 18)
	pdf.CellFormat(0, 12, "Отчёт по сбору средств", "", 1, "L", false, 0, "")

	pdf.SetFont("DejaVu", "", 10)
	pdf.SetTextColor(120, 120, 120)
	pdf.CellFormat(0, 6, "Период: "+periodLabel, "", 1, "L", false, 0, "")
	pdf.SetTextColor(0, 0, 0)
	pdf.Ln(4)

	// ===== Таблица по картам/наличным (сводная) =====
	cardTotals := map[string]float64{}
	var grandTotal float64
	for _, s := range summaries {
		grandTotal += s.Total
		for card, amt := range s.ByCard {
			cardTotals[card] += amt
		}
	}

	pdf.SetFont("DejaVu", "B", 13)
	pdf.SetTextColor(26, 60, 110)
	pdf.CellFormat(0, 8, "1. Сбор по картам / наличным", "", 1, "L", false, 0, "")
	pdf.SetTextColor(0, 0, 0)
	pdf.Ln(2)

	drawTableHeader(pdf, []string{"Куда", "Сумма, ₽"}, []float64{130, 40})

	cardKeys := sortedKeys(cardTotals)
	fill := false
	for _, card := range cardKeys {
		drawRow(pdf, []string{card, formatMoney(cardTotals[card])}, []float64{130, 40}, fill)
		fill = !fill
	}
	drawTotalRow(pdf, "ИТОГО", formatMoney(grandTotal), []float64{130, 40})

	pdf.Ln(8)

	// ===== Таблица по людям =====
	pdf.SetFont("DejaVu", "B", 13)
	pdf.SetTextColor(26, 60, 110)
	pdf.CellFormat(0, 8, "2. Сбор по людям", "", 1, "L", false, 0, "")
	pdf.SetTextColor(0, 0, 0)
	pdf.Ln(2)

	drawTableHeader(pdf, []string{"Имя", "Кол-во платежей", "Сумма, ₽"}, []float64{90, 40, 40})
	fill = false
	sortedSummaries := make([]db.ContactSummary, len(summaries))
	copy(sortedSummaries, summaries)
	sort.Slice(sortedSummaries, func(i, j int) bool {
		return sortedSummaries[i].Total > sortedSummaries[j].Total
	})
	for _, s := range sortedSummaries {
		drawRow(pdf, []string{s.CanonicalName, fmt.Sprintf("%d", s.Count), formatMoney(s.Total)}, []float64{90, 40, 40}, fill)
		fill = !fill
	}
	drawTotalRow(pdf, "ИТОГО", formatMoney(grandTotal), []float64{130, 40})

	return pdf.OutputFileAndClose(outPath)
}

// Section — один блок произвольного PDF-отчёта: заголовок, колонки и строки.
// Используется для отчётов, которые бот собирает по запросу ("кто сколько
// чеков скинул", "сделай отчёт с такими-то данными").
type Section struct {
	Title   string     // заголовок блока (необязательно)
	Columns []string   // названия колонок
	Rows    [][]string // строки; длина каждой = len(Columns)
	// TotalLabel/TotalRow — необязательная итоговая строка. Если TotalRow
	// задан, рисуется выделенная строка итога (длина = len(Columns)).
	TotalRow []string
}

// GenerateCustom строит PDF из произвольных секций-таблиц. Первая колонка
// выравнивается влево, остальные вправо (обычно там числа). Ширины колонок
// распределяются автоматически по ширине страницы.
func GenerateCustom(title, subtitle string, sections []Section, fontDir, outPath string) error {
	pdf := gofpdf.New("P", "mm", "A4", fontDir)
	pdf.AddUTF8Font("DejaVu", "", "DejaVuSans.ttf")
	pdf.AddUTF8Font("DejaVu", "B", "DejaVuSans-Bold.ttf")
	pdf.AddPage()

	pdf.SetFont("DejaVu", "B", 18)
	pdf.CellFormat(0, 12, title, "", 1, "L", false, 0, "")

	if subtitle != "" {
		pdf.SetFont("DejaVu", "", 10)
		pdf.SetTextColor(120, 120, 120)
		pdf.CellFormat(0, 6, subtitle, "", 1, "L", false, 0, "")
		pdf.SetTextColor(0, 0, 0)
	}
	pdf.Ln(4)

	for i, sec := range sections {
		if len(sec.Columns) == 0 {
			continue
		}
		if sec.Title != "" {
			pdf.SetFont("DejaVu", "B", 13)
			pdf.SetTextColor(26, 60, 110)
			pdf.CellFormat(0, 8, fmt.Sprintf("%d. %s", i+1, sec.Title), "", 1, "L", false, 0, "")
			pdf.SetTextColor(0, 0, 0)
			pdf.Ln(2)
		}

		widths := columnWidths(len(sec.Columns))
		drawTableHeader(pdf, sec.Columns, widths)
		fill := false
		for _, row := range sec.Rows {
			drawRow(pdf, padRow(row, len(sec.Columns)), widths, fill)
			fill = !fill
		}
		if len(sec.TotalRow) > 0 {
			drawTotalRowCells(pdf, padRow(sec.TotalRow, len(sec.Columns)), widths)
		}
		pdf.Ln(8)
	}

	return pdf.OutputFileAndClose(outPath)
}

// columnWidths распределяет 170 мм (ширина A4 за вычетом полей) по колонкам:
// первая (обычно имя/описание) шире, остальные равны между собой.
func columnWidths(n int) []float64 {
	const total = 170.0
	if n <= 1 {
		return []float64{total}
	}
	first := total * 0.45
	rest := (total - first) / float64(n-1)
	widths := make([]float64, n)
	widths[0] = first
	for i := 1; i < n; i++ {
		widths[i] = rest
	}
	return widths
}

func padRow(row []string, n int) []string {
	if len(row) >= n {
		return row[:n]
	}
	out := make([]string, n)
	copy(out, row)
	return out
}

// drawTotalRowCells рисует выделенную итоговую строку из готовых ячеек.
func drawTotalRowCells(pdf *gofpdf.Fpdf, values []string, widths []float64) {
	pdf.SetFont("DejaVu", "B", 10)
	pdf.SetFillColor(255, 243, 205)
	for i, v := range values {
		align := "L"
		if i > 0 {
			align = "R"
		}
		pdf.CellFormat(widths[i], 8, v, "1", 0, align, true, 0, "")
	}
	pdf.Ln(-1)
}

func drawTableHeader(pdf *gofpdf.Fpdf, headers []string, widths []float64) {
	pdf.SetFont("DejaVu", "B", 10)
	pdf.SetFillColor(26, 60, 110)
	pdf.SetTextColor(255, 255, 255)
	for i, h := range headers {
		align := "L"
		if i > 0 {
			align = "R"
		}
		pdf.CellFormat(widths[i], 8, h, "1", 0, align, true, 0, "")
	}
	pdf.Ln(-1)
	pdf.SetTextColor(0, 0, 0)
}

func drawRow(pdf *gofpdf.Fpdf, values []string, widths []float64, fill bool) {
	pdf.SetFont("DejaVu", "", 10)
	if fill {
		pdf.SetFillColor(238, 242, 248)
	} else {
		pdf.SetFillColor(255, 255, 255)
	}
	for i, v := range values {
		align := "L"
		if i > 0 {
			align = "R"
		}
		pdf.CellFormat(widths[i], 7, v, "1", 0, align, true, 0, "")
	}
	pdf.Ln(-1)
}

func drawTotalRow(pdf *gofpdf.Fpdf, label, total string, widths []float64) {
	pdf.SetFont("DejaVu", "B", 10)
	pdf.SetFillColor(255, 243, 205)
	totalWidth := 0.0
	for _, w := range widths[:len(widths)-1] {
		totalWidth += w
	}
	pdf.CellFormat(totalWidth, 8, label, "1", 0, "L", true, 0, "")
	pdf.CellFormat(widths[len(widths)-1], 8, total, "1", 0, "R", true, 0, "")
	pdf.Ln(-1)
}

func formatMoney(v float64) string {
	// Разбивка на разряды пробелом: 583000 -> "583 000"
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.0f", v)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	res := string(out)
	if neg {
		res = "-" + res
	}
	return res
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
