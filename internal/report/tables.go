package report

import (
	"fmt"
	"math"
	"strings"

	"github.com/jung-kurt/gofpdf"
)

// GenerateTables wraps long names and repeats table headers on every page.
// Wide employee lists use landscape A4; financial totals stay as supplied.
func GenerateTables(title, subtitle string, sections []Section, fontDir, outPath string) error {
	orientation := "P"
	for _, s := range sections {
		if len(s.Columns) > 4 {
			orientation = "L"
		}
	}
	pdf := gofpdf.New(orientation, "mm", "A4", fontDir)
	pdf.SetMargins(12, 12, 12)
	pdf.SetAutoPageBreak(false, 14)
	pdf.AddUTF8Font("DejaVu", "", "DejaVuSans.ttf")
	pdf.AddUTF8Font("DejaVu", "B", "DejaVuSans-Bold.ttf")
	pdf.SetFooterFunc(func() {
		pdf.SetY(-10)
		pdf.SetFont("DejaVu", "", 8)
		pdf.SetTextColor(100, 110, 125)
		pdf.CellFormat(0, 4, fmt.Sprintf("Страница %d", pdf.PageNo()), "", 0, "R", false, 0, "")
	})
	pdf.AddPage()
	w, h := pdf.GetPageSize()
	width := w - 24
	pdf.SetFillColor(26, 43, 74)
	pdf.Rect(12, 12, width, 30, "F")
	pdf.SetXY(17, 16)
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("DejaVu", "B", 17)
	pdf.CellFormat(width-10, 9, title, "", 1, "L", false, 0, "")
	pdf.SetX(17)
	pdf.SetFont("DejaVu", "", 9)
	pdf.MultiCell(width-10, 4.5, subtitle, "", "L", false)
	pdf.SetY(48)
	pdf.SetTextColor(25, 35, 50)
	for _, sec := range sections {
		if len(sec.Columns) == 0 {
			continue
		}
		if pdf.GetY() > h-50 {
			pdf.AddPage()
		}
		pdf.SetFont("DejaVu", "B", 11)
		pdf.MultiCell(width, 5, sec.Title, "", "L", false)
		pdf.Ln(2)
		pdf.SetFont("DejaVu", "", 9)
		widths := tableColumnWidths(pdf, sec, width)
		draw := func(values []string, bold bool, fill [3]int) {
			style := ""
			if bold {
				style = "B"
			}
			pdf.SetFont("DejaVu", style, 9)
			values = padRow(values, len(widths))
			height := tableRowHeight(pdf, values, widths)
			x, y := 12.0, pdf.GetY()
			pdf.SetFillColor(fill[0], fill[1], fill[2])
			pdf.SetDrawColor(205, 213, 225)
			for i, v := range values {
				pdf.Rect(x, y, widths[i], height, "DF")
				pdf.SetXY(x+1.5, y+1.5)
				align := "L"
				if strings.Contains(sec.Columns[i], "₽") || strings.Contains(sec.Columns[i], "шт.") {
					align = "R"
				}
				pdf.MultiCell(widths[i]-3, 4.5, v, "", align, false)
				x += widths[i]
			}
			pdf.SetXY(12, y+height)
		}
		header := func() {
			pdf.SetTextColor(255, 255, 255)
			draw(sec.Columns, true, [3]int{26, 60, 110})
			pdf.SetTextColor(25, 35, 50)
		}
		header()
		for i, row := range sec.Rows {
			pdf.SetFont("DejaVu", "", 9)
			if pdf.GetY()+tableRowHeight(pdf, padRow(row, len(widths)), widths) > h-15 {
				pdf.AddPage()
				header()
			}
			fill := [3]int{255, 255, 255}
			if i%2 == 1 {
				fill = [3]int{239, 243, 249}
			}
			draw(row, false, fill)
		}
		if len(sec.TotalRow) > 0 {
			pdf.SetFont("DejaVu", "B", 9)
			if pdf.GetY()+tableRowHeight(pdf, padRow(sec.TotalRow, len(widths)), widths) > h-15 {
				pdf.AddPage()
				header()
			}
			draw(sec.TotalRow, true, [3]int{255, 243, 205})
		}
		pdf.Ln(7)
	}
	return pdf.OutputFileAndClose(outPath)
}

func tableColumnWidths(pdf *gofpdf.Fpdf, sec Section, width float64) []float64 {
	weights := make([]float64, len(sec.Columns))
	minimum := make([]float64, len(sec.Columns))
	for i, name := range sec.Columns {
		minimum[i] = 16
		measure := func(value string) {
			for _, word := range strings.Fields(value) {
				minimum[i] = math.Max(minimum[i], math.Min(55, pdf.GetStringWidth(word)+5))
			}
		}
		measure(name)
		weights[i] = math.Max(18, math.Min(65, pdf.GetStringWidth(name)+5))
		for _, row := range sec.Rows {
			if i < len(row) {
				measure(row[i])
				weights[i] = math.Max(weights[i], math.Min(65, pdf.GetStringWidth(row[i])+5))
			}
		}
	}
	var minSum, extraSum float64
	for i, v := range weights {
		minSum += minimum[i]
		weights[i] = math.Max(1, v-minimum[i])
		extraSum += weights[i]
	}
	for i := range weights {
		if minSum >= width {
			weights[i] = width * minimum[i] / minSum
		} else {
			weights[i] = minimum[i] + (width-minSum)*weights[i]/extraSum
		}
	}
	return weights
}

func tableRowHeight(pdf *gofpdf.Fpdf, values []string, widths []float64) float64 {
	lines := 1
	for i, v := range values {
		lines = max(lines, len(pdf.SplitText(v, widths[i]-3)))
	}
	return float64(lines)*4.5 + 3
}
